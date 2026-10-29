# Changelog

All notable changes to Concord Voice are recorded in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
This file names the pending section `[Prerelease]`, not `[Unreleased]`, after
the release channel that ships it first.

To add an entry, write one line under the matching `###` category in
`[Prerelease]`: Added, Changed, Deprecated, Removed, Fixed, or Security. Start
the line with a bold area tag, such as `**Voice:**`. Keep one summary of 1-3
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

- **Billing:** Settings ▸ Subscription lists each plan with its monthly and yearly price and opens a secure Stripe page to subscribe or manage billing.
- **Billing:** New subscribers get a 30-day free trial of Supersonic. The card is collected first, and billing starts when the trial ends. Wing has no trial.
- **Billing:** Subscribers can change plans in Settings, see the amount due before an immediate switch, and withdraw a change that is booked for renewal.
- **Billing:** A Wing subscription can share Supersonic with one friend. Invite the friend from Settings ▸ Subscription. You can change your +1 once every 30 days.
- **Billing:** A premium code redeemed on a paid subscription becomes account credit that your next renewals use first. A code for a different plan shows the credit and asks you to confirm.
- **Servers:** Owners can require MFA for dangerous actions from Server Settings. Members then confirm each dangerous action with a code or security key. API: send `mfa_code` on a retry after a 403 `mfa_required`. `GET /api/v1/server/capabilities` reports `features.mfaEnforcedDangerousActions`.
- **Servers:** Delete Role asks you to confirm and names the role first.
- **Messages:** Every purge has an "Include pinned messages" option, off by default. API: `include_pinned` and `features.purgeKeepsPinned`.
- **Messages:** Message expiration timers can keep pinned messages, and new timers keep them by default. API: `include_pinned` and `features.expirationPinnedPolicySupported`.
- **Messages:** Up Arrow in an empty message box edits your last message.
- **Voice:** Auto Calibrate and Manual Calibrate can suggest a fixed microphone gate threshold from a room-and-voice check. Manual Calibrate also shows a live meter.
- **Operations:** Blue/green media deployment can move new voice rooms to a second instance while existing rooms drain. It is off by default.
- **Operations:** A separate West staging deployment path is available. It stays off until its secrets, access, and checks are complete. West is not a production site.
- **Operations:** Groundwork, off by default: managed ingress verification, managed egress, Redis credential rotation, TURN credential rotation, and shared media listeners. Each needs host checks and approval before activation.

### Changed

- **Plans:** Every account can create any number of servers, search as far back as a subscriber, and customize theme colors. One account can create up to 10 servers in 24 hours.
- **Web:** Opening the web address in a browser shows a link to download the desktop app, not an "Update Required" prompt.
- **Search:** Message search looks back 90 days or 600 messages at a time, loads older results as you scroll, and says when it is paused or stopped.
- **Search:** Jump reaches messages that are not loaded or are in another channel, from search results, pins, and reply quotes.
- **Settings:** Subscription is under Account, protection settings are under Security, and device permissions are under About & Updates.
- **Settings:** Sections start collapsed and remember what you expand while you use Settings and for 30 minutes after you leave.
- **Settings:** Audio and video device controls sit with their settings. An app-only Microphone Level (0–200%) shows when automatic gain control (AGC) is off.
- **Security:** Purge, purge protection, DM Clear, and Security settings prompts ask only for the methods your account uses. A passkey or security key works in one step.
- **Privacy:** New accounts start with stricter defaults for friend requests, voice details, call presence, and notification previews. Existing choices do not change.
- **Friends:** Under Mutual Servers, people who share a server or a mutual friend with you, or who have your friend code, can send you a request. Username changes wait six months on Free and three on Premium.
- **Recovery:** Recovery Circle recovery is temporarily paused. Your saved contacts stay in Settings. Use another recovery method to regain access.
- **Voice:** The Noise Gate has Dynamic, Auto Calibrate, Manual Calibrate, and Off modes. Dynamic adapts during a call or Microphone Test.
- **Voice:** With AGC on, calls and Microphone Test limit outgoing microphone sample peaks to −6 dBFS through one shared processing path.
- **Voice:** Turning DTX off in Advanced settings disables it for every quality preset.
- **Voice:** Voice, camera, and screen sends use separate plan-based bitrate ceilings. If video use stays over the limit, the server pauses video for a cooldown and voice continues.
- **Voice:** When voice is paused for server maintenance, joining a call says so and suggests that you try again in a few minutes.
- **Voice:** When operators enable blue/green mode, calls recover after the old media instance stops at its two-hour drain cutoff.
- **Video:** Enable HDR and Prioritize HDR Codecs are separate settings. Neither setting guarantees HDR capture or display.
- **Video:** Webcam speaker frames keep a fixed size and show a steady highlight.
- **GIFs:** GIF personalization sends KLIPY an ID derived from your Personalization ID. With personalization off, GIF browsing uses a temporary ID that changes about every 30 minutes.
- **Bug reports:** Screenshots have storage allowances and expire after 72 hours if the report is abandoned.
- **Desktop:** Electron 44.5.0, Electron Forge 8 packaging, lucide-react 1.48.0, mediasoup-client 3.24.1, and socket.io-client 4.8.4.
- **Server:** The control plane builds with Go 1.27.2.

### Fixed

- **Billing:** Blocking someone ends a Wing seat between you, whichever side blocks. Unfriending or blocking also cancels a pending seat invite at once; unfriending alone keeps an active seat.
- **Billing:** Switching from a monthly to a yearly plan works again. It credits the unused part of the month and keeps your billing date.
- **Billing:** Subscribing twice at the same time no longer creates a second subscription.
- **Billing:** Opening billing no longer withdraws a booked plan change. Cancellation keeps access through the paid period, and the plan card shows your actual price.
- **Billing:** Redeeming two codes at the same time no longer fails.
- **Billing:** Subscription updates apply in order, so an older downgrade cannot follow a newer upgrade.
- **Billing:** Recovery changes only subscriptions that its environment owns, and canceled seat requests still notify members.
- **Chat:** The desktop chat connection no longer drops and reconnects every 14 minutes.
- **Chat:** Chat no longer drops for everyone every 30 seconds when someone sits in a voice channel that none of their friends can see.
- **Chat:** Changing activity sharing settings in large servers no longer disconnects unrelated users.
- **Chat:** Chat stays responsive under excessive connection traffic.
- **Messages:** Failed channel messages keep their text and retry controls after a brief disconnect.
- **Messages:** History no longer skips messages that share a timestamp, in channels or DMs.
- **Messages:** An edit is held to the character limit.
- **Messages:** A delayed failed edit no longer closes an edit in another chat panel or loses its text.
- **Messages:** Attachments upload and send in call text chat.
- **Messages:** Attachments that scroll out of view release their decrypted data.
- **Messages:** A server purge refreshes only the channels it changed.
- **Messages:** Reading a server's channels clears its unread indicator.
- **Invites:** Invite links keep their previews and open the selected invite after sign-in. Profile and server images load through the signed-in account.
- **Friends:** Copying a friend code no longer leaves a timer running after you leave Add Friend.
- **Roles:** Failed role and override saves show an error. A purge refused for MFA asks for a code or links to setup.
- **Voice:** Calls keep your mute and deafen choices through recovery, and DM calls keep their call type.
- **Voice:** Dynamic gating releases stale thresholds and keeps phrase evidence through pauses. Invalid saved thresholds reset to the default.
- **Voice:** The microphone resumes after a settings change with the Dynamic noise gate.
- **Voice:** If AGC fails, the microphone stays paused.
- **Voice:** Joins capture the selected microphone once and explain how to recover if capture fails.
- **Voice:** Audio menus show one Default option that follows the operating system.
- **Voice:** Join errors show in a small centered prompt and no longer push the layout down.
- **Voice:** Removing someone from voice during their join waits for their connection to close.
- **Voice:** Disconnects wait for media cleanup before the session is released.
- **Voice:** Voice moderation and group DM removals survive broker outages.
- **Voice:** Voice startup waits for enforcement. If enforcement fails, new joins stop and calls close within 30 seconds.
- **Voice:** Blue/green retries keep call and room ownership.
- **Video:** Video stays on when someone with incompatible video support joins.
- **Video:** H.264 video no longer goes black after another camera turns on.
- **Video:** Camera swaps keep local previews, and late media from an earlier session is closed.
- **Video:** Camera recovery uses codec and HDR settings that you save during a restart.
- **Sign-in:** Email-code sign-in sends the code.
- **Sign-in:** Sign-in offers backup codes only when they can work.
- **Sign-in:** Session checks can use your security key.
- **Settings:** Verification setup no longer loses unsaved settings or applies a stale confirmation.
- **Accessibility:** Settings uses a native microphone level meter and respects reduced motion. Screen readers skip decorative security icons.
- **Desktop:** Pilot builds can connect alongside Stable builds.
- **Desktop:** Windows updates recheck the installer before restart, including saved downloads.
- **Desktop:** Group DM icons load through authenticated requests.
- **Server:** Animated image uploads use less memory.
- **Self-hosting:** Calls through the bundled TURN relay work when the relay and media server share one public address. An IPv6 loopback rule no longer blocks IPv4 peers.
- **Self-hosting:** The offline ingress verifier refuses ambiguous NAT address and source-port pools instead of reading them as fixed translations.
- **Operations:** A production deploy keeps running on the server when its SSH tunnel drops mid-rollout, and the deploy reconnects to that run instead of failing.

### Security

- **Accounts:** Sign-in protection accepts a key re-encryption only from the maintenance run that started it. It refuses to roll back while its state is unknown.
- **Accounts:** An account with suspicious session use stays blocked until session revocation finishes.
- **Accounts:** Account recovery asks for the authenticator you reserved for recovery.
- **Accounts:** Trusted-device recovery requires matching fingerprints on both devices. Update both devices and start a new request.
- **Accounts:** Security-key confirmations go only to the server that asked.
- **Accounts:** Session refresh checks the device identifier. Concurrent sign-ins cannot reuse a recovery code.
- **Desktop:** The desktop rechecks its saved web app when the signature changes and refuses an invalid one.
- **Desktop:** Electron 44.9.1 blocks compromised eMudhra intermediate certificates.
- **Real time:** Control connections close when their access token expires.
- **Real time:** An account can keep up to 8 WebSocket connections and a process accepts up to 10,000.
- **Real time:** Profile changes reach only people with a current relationship. Account deletion clears status only for its prior viewers.
- **Messages:** DMs store participant messages only as encrypted messages.
- **Messages:** The server requires an explicit key version on every message. Upgrade all control-plane writers before you apply the migration.
- **Messages:** Attachment deletion requires current access to the chat. Group DM icon uploads and reads require current admin role or membership.
- **Messages:** Erased feedback screenshots no longer load from the image endpoint.
- **Plans:** Animated avatars, banners, and server icons follow the tier rules on every route.
- **Plans:** Voice refuses microphone streams that do not declare packet time or bitrate. Repeated over-limit requests are flagged and resync your plan limits.
- **Self-hosting:** Disabled feedback refuses submissions, and the installer rejects unsafe settings and ambiguous retained data. Release publication verifies proof and attestations.
- **Operations:** Redis health checks keep passwords out of Docker event commands. Existing deployments need the updated container configuration.
- **Operations:** CI fails on fixable high or critical vulnerabilities in server images.
- **Dependencies:** Go 1.27.3 ([GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809)), Go 1.26.9 and `golang.org/x/net` 0.60.0, and Vite 8.3.3 ([GHSA-rq7h-c2jc-7f22](https://github.com/vitejs/vite/security/advisories/GHSA-rq7h-c2jc-7f22), [GHSA-vfpm-58rq-9qcg](https://github.com/vitejs/vite/security/advisories/GHSA-vfpm-58rq-9qcg), [GHSA-9jrq-w75r-8gcw](https://github.com/vitejs/vite/security/advisories/GHSA-9jrq-w75r-8gcw)).
- **Dependencies:** `source-map-js` 1.2.2 ([CVE-2026-93749](https://github.com/advisories/GHSA-68fv-2mgg-jv7q)), `shell-quote` 1.12.0 ([CVE-2026-102422](https://github.com/advisories/GHSA-pqg4-j6r4-53mv)), media-plane forwarded-address parser, and `sprintf-js` removed.

## [0.2.48] - 2026-10-04

Screen sharing stays steady when a capture changes, and voice pauses a stream
that sends far more than your plan allows. Security prompts ask for the right
proof in the right places, never a code your account cannot provide and never
your password twice.

**88 Total Changes** | 2 Added | 16 Changed | 57 Fixed | 13 Security

### Added

- **Bug reports:** The bug-report form accepts up to four screenshots (PNG, JPEG, or WebP, 5 MB each). Re-encoding removes camera metadata such as GPS location.
- **Messages:** Deleting more than 15 messages in 30 seconds, or 100 in 24 hours, asks you to confirm it is you. API: optional `{"mfa_code", "step_up_token"}` body and new `POST /api/v1/auth/step-up/password` (migration 000162).

### Changed

- **Voice:** The voice server pauses a microphone, camera, or screen share that keeps sending far above your plan. A second time within an hour removes you from the call for 15 minutes.
- **Voice:** Screen sharing with sound sends at your plan's audio quality (96 kbps on Free).
- **Messages:** API clients can hide a DM or group from one participant's list, or clear that participant's history before a server-stamped cutoff.
- **Messages:** DMs offer separate Hide, Clear history for me, and Leave group controls.
- **Messages:** Purge Messages uses one range menu and says first that a DM purge removes only your own messages.
- **Messages:** The Hide confirmation no longer uses warning styling, and Leave group has its own section in Group Info.
- **Servers:** Owners and Administrators can require MFA for dangerous actions through `PUT /api/v1/servers/{id}/mfa-enforcement`. Email and text codes do not count.
- **Appearance:** Font Size moved to Appearance ▸ Application Font and stays usable with Dyslexic Support on.
- **Accessibility:** UI Scale runs from 50% to 200% and zooms the whole interface. Above 100%, the window width limits the zoom.
- **Appearance:** Application Font has two modes. One Font applies your pick everywhere, and Font by Area sets fonts for Messages, Headings, Navigation, and Interface.
- **Appearance:** Application Font highlights your mode. Theme Default and Concord Voice Default each have a description.
- **Security:** After you add an authenticator app, Concord says whether it created, kept, or failed to create your recovery key.
- **Server:** DM-block and credential-epoch cleanup records retryable voice ejections and fences stale callbacks.
- **Server:** DM writes recheck current membership before they store keys or uploads.
- **Server:** Server changes serialize authority and membership at their write boundary, so stale operations fail closed.
- **Voice:** Voice joins verify admission before they open a session, so an old disconnect cannot remove a newer join.

### Fixed

- **Bug reports:** Screenshots removed while logs are collected are no longer sent with the report.
- **Messages:** Messages and attachments always carry the version of the key that encrypted them.
- **Messages:** A purge target names the same author at admission and deletion. A malformed `target_user_id` returns 400.
- **Messages:** A hidden conversation comes back as soon as someone messages it.
- **Messages:** Group messages from before someone left are readable again.
- **Messages:** Purging a DM stops after 10 seconds, and removed messages disappear for others at once.
- **Messages:** Empty conversations that everyone has hidden are cleaned up, and the last group member can leave.
- **Messages:** Return to Latest stays at the latest message while older history loads.
- **Messages:** A saved chat position waits for the history page that holds it.
- **Self-hosting:** Self-hosted servers keep Google and Apple sign-in unavailable. Password sign-in still works.
- **Sign-in:** Remember Me keeps your saved sign-in when the keychain is briefly unavailable.
- **Sign-in:** Two-factor sign-in no longer hangs when your encryption keys cannot be unlocked.
- **Security:** Email MFA can be turned on again, and SMS shows as coming soon.
- **Security:** Regenerating backup codes from Settings works again.
- **Security:** Try again on the recovery-key screen counts each attempt.
- **Security:** Reset TOTP accepts a typed backup code.
- **Security:** Turning on two-factor authentication no longer asks for your code twice.
- **Security:** A damaged two-factor record no longer crashes sign-in checks or the `mfa-rekey` tool.
- **Security:** Accounts with only email or text codes are not asked for a code they cannot check.
- **Security:** Removing your last authenticator or security key while email or text codes are on is refused, with a reason.
- **Security:** Purging messages no longer asks for your password twice.
- **Security:** A wrong password or code marks the field that was wrong, and screen readers announce it.
- **Security:** Retrying a lost recovery-key save resends the same key.
- **Security:** A temporarily unavailable verification check says so instead of "too many attempts".
- **Security:** Account-security actions report a failure instead of claiming success.
- **Security:** The "Verify Your Identity" prompt opens on top of Settings and takes the keyboard.
- **Servers:** Requests to a server that is deleted mid-request answer as for a non-member, not with a server error.
- **Servers:** Channel and category permission overrides save again. API: permission bitfields now cross JSON as decimal strings (breaking change).
- **Servers:** Server owners can mention @all and @here again.
- **Servers:** Text-only channel changes work on busy voice servers.
- **Invites:** Inviting someone to a server they are already in is greyed out.
- **Invites:** The invite button in a DM lists every server you can invite to.
- **Invites:** An invite to a server you are already in says "Joined".
- **Voice:** A failed voice join no longer removes someone who joined the next call.
- **Voice:** Voice access cleanup no longer sends an in-flight update to someone whose access was revoked.
- **Voice:** Voice activity clears before a privacy change reconnects the app.
- **Voice:** Moving between voice channels on different servers no longer loses the old-channel leave.
- **Voice:** Voice channel lists recover after stale sessions without wrong counts or repeated leave sounds.
- **Presence:** Rich Presence sharing settings save every time.
- **Screen share:** Free accounts start screen sharing at 1080p, not a Premium resolution.
- **Screen share:** Sharing an app with sound says when the sound could not be captured.
- **Screen share:** App audio captures helper processes and says when the sound stops.
- **Screen share:** Switching share sources no longer drops or leaks per-app audio.
- **Desktop:** A malformed server response no longer replaces the desktop's working configuration.
- **Desktop:** Update, reconnection, and what's-new windows work while Settings is open.
- **Desktop:** Links opened from a picture-in-picture window open in your browser.
- **Desktop:** Right-click menus no longer open partly off-screen.
- **Appearance:** Brand-colored buttons and avatars meet 4.5:1 contrast in every color scheme.
- **Appearance:** Premium labels and Admin badges are readable in every color scheme.
- **Appearance:** The sign-in logo is readable in light themes.
- **Appearance:** Dyslexic Support changes every piece of text.
- **Appearance:** Your app font reaches text boxes, buttons, and menus.
- **Accessibility:** Error text in code and security-key prompts is easier to read.
- **Accessibility:** The Basic / Advanced switch in Audio and Video Configuration works with a keyboard and screen reader.
- **Accessibility:** The audio quality labels work as buttons for keyboard and screen-reader users.
- **Accessibility:** Keyboard focus rings are visible in every theme and no longer look like a selection.
- **Operations:** A dirty database migration stops control-plane startup until an operator recovers it.

### Security

- **Accounts:** A backup code no longer signs you in past an authenticator app set to recovery only.
- **Accounts:** An authenticator-app code works only once.
- **Accounts:** A security-key confirmation counts only for the action you confirmed.
- **Accounts:** Weakening your account's recovery asks you to prove it is you.
- **Accounts:** Two-factor sign-in keeps its attempt limits, and your code, through a server fault.
- **Accounts:** Account safety checks and moderation agree on one spelling of your account's identifier.
- **Messages:** Once every member has cleared a DM or group's history, the server deletes those messages and attachments.
- **Presence:** A dropped voice connection no longer leaves your activity visible to people you later hide it from.
- **Operations:** The control plane treats an unset `ENVIRONMENT` as production, and refuses `CONCORD_ENV=test` outside development or test.
- **Operations:** Identity-anomaly audit records keep the request's final authorization result.
- **Dependencies:** The voice server's connection library closes GHSA-2GC4-CQFQ-P2GV.
- **Dependencies:** Electron 44.4.3 carries current Chromium, V8, and PDFium security fixes.
- **Dependencies:** macOS installer builds no longer pull the legacy DMG dependency chain.

---

## [0.2.47] - 2026-09-18

Screen sharing gets tabs, a sound switch, per-app audio, and in-place source
switching. Shared chats can set message timers, and Rich Presence shows what
is shared and who may receive it.

**62 Total Changes** | 13 Added | 5 Changed | 37 Fixed | 7 Security

### Added

- **Messages:** Changing the message timer leaves a note in the conversation that says who changed it and to what.
- **Messages:** The server stores message expiry policy for channels, DMs, and group DMs, and applies it in resumable batches.
- **Messages:** Eligible chats can set message expiration to Off, 1 hour, 24 hours, 7 days, or 30 days, for new or existing messages.
- **Messages:** Opening a busy channel or DM drops you at your first unread message, and messages you watch arrive count as read.
- **Presence:** Rich Presence activity shows in member rows and profiles, and your panel shows its eligible audience.
- **Screen share:** The share picker has Screens and Windows tabs with thumbnail grids.
- **Screen share:** Screen sharing carries your computer's sound by default, with a switch to turn it off. Linux does not support it yet.
- **Screen share:** Sharing one window can send only that app's sound, where the operating system supports it.
- **Screen share:** A Switch button changes what you share without dropping viewers.
- **Voice:** Each person's tile has a one-click mute that only you hear.
- **Voice:** Voice participants show their profile photo in the channel sidebar.
- **GIFs:** GIFs stop playing when you switch away from Concord, including animated WebP and PNG.
- **GIFs:** Settings ▸ Accessibility ▸ Display has a GIF Playback control: Auto, Always, or Hover only.

### Changed

- **Messages:** Message expiration and purging are separate controls. The timer shows next to the encryption notice, and Purge has its own header button.
- **Messages:** Expired messages leave chats and pinned views together, in five-minute sweeps.
- **Voice:** Call buttons are grouped by what they do, and only Leave is red.
- **Voice:** Picture-in-Picture and the stay-active setting moved into a More menu.
- **Voice:** Busy servers give each new voice room to the least busy CPU core.

### Fixed

- **Voice:** Per-person volume, master output volume, quiet-user boost, and output device choice work again.
- **Voice:** The chat button is back in DM calls.
- **Voice:** Right-clicking someone in a DM call opens their menu.
- **Voice:** Voice no longer drops people who are still connected when a media server's clock runs ahead.
- **Voice:** A media server with a badly wrong clock can no longer freeze the rooms it serves.
- **Video:** Turning your camera on in a call works again.
- **Video:** Video drops to a smaller size before it freezes when your computer is struggling.
- **Video:** Joining a call late no longer leaves your camera stuck at one quality.
- **Video:** The controls on a video can be reached with a keyboard.
- **Screen share:** Screen sharing respects your quality setting again.
- **Screen share:** Changing screen-share quality mid-share reaches everyone watching.
- **Screen share:** Screen sharing on macOS 14.2 and later carries sound. macOS 12 and earlier share picture only.
- **Screen share:** The share window keeps keyboard focus until you choose or close it.
- **Screen share:** The screen-share volume slider shows only when you point at the video.
- **Messages:** Rotating a DM's encryption key no longer locks everyone out of the conversation.
- **Messages:** A DM with only a photo or file says what it is in your DM list, not "Encrypted message".
- **Messages:** Chats come back to where you left them, and Return to Latest stays at the bottom.
- **Notifications:** The unread count on the app icon clears when you read the messages.
- **Notifications:** The app icon stops bouncing after a notification.
- **Presence:** If you drop offline, you come back on your own.
- **Presence:** You no longer drop offline or lose your connection while Concord sits in the background.
- **Presence:** Going invisible no longer knocks everyone else offline.
- **Presence:** "Eligible audience" shows only when you point at your user area.
- **Sign-in:** SSO sessions survive refresh when optional session metadata is missing.
- **GIFs:** The GIF and emoji pickers sit against their buttons with an arrow, and are about 15% larger.
- **GIFs:** Saving a GIF no longer sends you back to the top.
- **Appearance:** Secondary text throughout the app is readable, including in custom themes.
- **Appearance:** Midnight Sky's secondary text is readable on raised panels.
- **Appearance:** The DM voice-call record is readable in every theme.
- **Appearance:** 23 fixed colors now follow your theme in light themes.
- **Appearance:** Accent-colored buttons keep a readable label, High Contrast included.
- **Appearance:** Text on red and green buttons is legible in every theme.
- **Appearance:** Server-enforced mutes show amber again, and missing hover highlights are back.
- **Appearance:** The documents in Settings ▸ About have their dividers and boxes.
- **Appearance:** Call buttons and badges follow your theme.
- **Desktop:** The DM profile window opens in the middle of the screen.
- **Desktop:** The installed app is about 37 MB smaller and no longer ships its source code or an internal test config.

### Security

- **Messages:** Unread counts no longer leak from channels you can see but cannot read.
- **Desktop:** Screen-share audio loads its native helper directly and refuses a substituted JavaScript module.
- **Voice:** Voice rooms no longer stay full after everyone has left.
- **Operations:** The control plane and media plane emit privacy-safe security events for Nightwatch ingestion.
- **Sign-in:** Release-attestation tokens accept only the one signature algorithm GitHub uses.
- **Sign-in:** Google and Apple sign-in accept only the exact signature type those providers use.
- **Dependencies:** Desktop and Admin resolve patched Undici releases, and Worker deployments use the audited lockfile's Wrangler.

---

## [0.2.46] - 2026-09-07

Voice connects on networks that block UDP, and Picture-in-Picture voice windows
get a private line and a one-time credential. Server updates no longer cut your
connection short, and a broken update can no longer pass as a success.

**9 Total Changes** | 7 Fixed | 2 Security

### Fixed

- **Voice:** Voice works on networks that block UDP, through the TCP and TLS relays the server offers.
- **Voice:** Calls no longer spend setup time on a TCP route that no server accepts. Operators can still enable it.
- **Voice:** Closing Picture-in-Picture voice during startup disconnects it cleanly.
- **Presence:** Your voice and call status no longer disappears every hour.
- **Server:** Server updates no longer cut your connection short.
- **Operations:** An update that cannot reach its database fails its health check instead of passing.
- **Messages:** Deleting a message retires its unshared attachment.

### Security

- **Voice:** Picture-in-Picture voice windows get their own one-time credential and private channel.
- **Messages:** Group deletion rechecks the administrator under lock.

## [0.2.45] - 2026-09-02

Voice channels and private calls can be joined again. The desktop runtime moves
to Electron 43.5.1, and sign-in rejects extension data that Concord did not
request.

**8 Total Changes** | 5 Changed | 1 Fixed | 2 Security

### Changed

- **Desktop:** Electron 43.5.1 brings upstream Chromium, Node.js, and V8 fixes.
- **Messages:** Large encrypted attachments can use consistently sized upload pieces on compatible clients.
- **Video:** Video forwarding keeps temporal quality at or below the requested layer.
- **Dependencies:** `lucide-react` and the media-plane `qs` dependency moved to reviewed releases.
- **Dependencies:** Development and CI tooling was refreshed without runtime changes.

### Fixed

- **Voice:** Voice channels and calls can be joined again. The minimum-version check no longer applies to internal service calls.

### Security

- **Sign-in:** WebAuthn sign-in and enrollment reject client extension output that Concord did not request.
- **Dependencies:** The control plane drops the advisory-affected MongoDB driver release and updates `quic-go` and `sonic`.

## [0.2.44] - 2026-09-01

You can choose who sees your voice-channel and private-call activity. The
attachment stack is ready for the hosted storage cutover, and deletions keep
the media and activity cleanup they trigger.

**17 Total Changes** | 2 Added | 8 Changed | 7 Fixed

### Added

- **Presence:** Settings ▸ Rich Presence has audience and detail controls for server voice and private calls.
- **Server:** The server can require a minimum desktop version before attachment format changes.

### Changed

- **Presence:** The desktop keeps voice and call activity current internally.
- **Messages:** Encrypted attachment uploads negotiate their storage format with the server.
- **Operations:** Production validates the R2 attachment destination before cutover.
- **Desktop:** Desktop utilities are organized by concern, with no behavior change.
- **Dependencies:** `jose`, `lucide-react`, and the build-only `xmldom` resolution moved to reviewed releases.
- **Dependencies:** Admin, desktop, and media-plane development tooling was refreshed.
- **Dependencies:** Browser compatibility metadata no longer resolves to advisory-affected releases.
- **Operations:** Weekly dependency triage fails closed on missing PR CI.

### Fixed

- **Presence:** Deleting a server no longer leaves its voice activity visible after a restart.
- **Presence:** Removing someone from a private conversation, or deleting its creator, no longer leaves stale voice activity.
- **Messages:** Removing someone from a private conversation no longer deadlocks with a concurrent account deletion.
- **Messages:** Encrypted attachments stay on the compatible storage format during rollouts.
- **Messages:** Profile-media cleanup survives storage failures.
- **Messages:** Large encrypted attachments are prepared for consistently sized upload pieces.
- **Invites:** An invite preview no longer shows details from a different invite while it loads.

## [0.2.43] - 2026-08-30

The app can reach the servers again after a certificate change locked every
installed copy out. Deleting your account removes the files it uploaded.

**5 Total Changes** | 1 Changed | 1 Fixed | 3 Security

### Changed

- **Desktop:** Desktop state is grouped by what it controls, with no behavior change.

### Fixed

- **Desktop:** The app checks server certificates the way a browser does, so a provider certificate change no longer locks everyone out.

### Security

- **Desktop:** Switching between the app and the web version no longer leaves one unable to load your servers.
- **Accounts:** Deleting your account removes the files you uploaded.
- **Servers:** Changing a server's owner rechecks active voice access at once.

## [0.2.42] - 2026-08-29

Interface fixes reach you again without an app update. Invite links arrive,
open, and show the server, and the GIF picker's Categories and Recent tabs
work.

**9 Total Changes** | 7 Fixed | 2 Security

### Fixed

- **Desktop:** Interface fixes reach you again without an app update.
- **Invites:** "Open in Concord" on an invite link works.
- **Invites:** An invite that arrived while you were offline appears when you return.
- **Invites:** A server you join from a link appears in the sidebar at once.
- **GIFs:** The GIF picker's Categories and Recent tabs show content.
- **GIFs:** The GIF picker keeps its size when a tab is empty.
- **Chat:** A burst of reconnections no longer holds up messages.

### Security

- **Presence:** Someone who has just lost access can no longer be shown that you are online.
- **Desktop:** Screen-capture protection reports itself as off where it cannot be enforced.

## [0.2.41] - 2026-08-27

Attachments can be as large as your plan says, you can choose who may send you
a friend request, and server roles can be reordered. Encrypted voice and video
are no longer garbled on recent engine builds.

**12 Total Changes** | 3 Added | 1 Changed | 6 Fixed | 2 Security

### Added

- **Messages:** Attachments up to 256 MiB upload in encrypted pieces.
- **Friends:** Settings ▸ Privacy sets who may send you a friend request.
- **Servers:** Server roles can be reordered by drag, keyboard, or toolbar, with Apply Order.

### Changed

- **Desktop:** Concord trusts a list of interface-signing keys, so a key change does not disable the cache.

### Fixed

- **Voice:** Voice and video are no longer garbled on recent engine builds.
- **Voice:** Picture-in-picture windows decrypt what they show.
- **Chat:** The connection no longer drops every few minutes.
- **Voice:** A voice server queue overflow no longer disconnects everyone on that machine.
- **Messages:** Mentions in DMs show names again.
- **Presence:** A restart no longer leaves your activity showing after you leave.

### Security

- **Friends:** A storage fault no longer reveals which friend codes exist.
- **Desktop:** An opt-in setting on macOS and Windows keeps Concord windows out of screen captures.

---

## [0.2.40] - 2026-08-20

Friend codes have a web page of their own. It shows who the code belongs to and
offers to open Concord, and it looks the same whether the code is live or not.

**8 Total Changes** | 1 Added | 1 Changed | 5 Fixed | 1 Security

### Added

- **Friends:** Friend code links open a page with the owner's name and avatar, and a button that opens Concord. Dead codes show a neutral page of the same shape.

### Changed

- **Presence:** A friend or privacy change can delay a Custom Status save for up to 30 seconds, with a `Retry-After` header.

### Fixed

- **Friends:** Several friend or invite links clicked quickly all open, in order.
- **Friends:** A brief server problem no longer reports a working friend code as dead.
- **Friends:** Revoking a friend code says when it fails.
- **Presence:** Leaving, or being kicked or banned from, a server stops your activity showing there at once.
- **Presence:** Deleting a server or your account no longer leaves your activity visible to others.

### Security

- **Friends:** A friend link clicked before sign-in no longer follows you into someone else's account.

## [0.2.39] - 2026-07-31

A self-hosted server address is checked when the app connects, not only when
you type it. A server on your own network needs your approval in a dialog that
the web layer cannot draw or click.

**1 Total Changes** | 1 Security

### Security

- **Self-hosting:** A compromised web layer can no longer make the app read your private network.

## [0.2.38] - 2026-07-30

Encrypted channel keys reach every member of a busy server without stalling or
handing out a revoked key. The DM and server sidebars share one adaptive layout.

**14 Total Changes** | 2 Changed | 3 Fixed | 9 Security

### Changed

- **Desktop:** The DM and server sidebars share one adaptive layout.
- **Dependencies:** mediasoup 3.22.0 and the current React release.

### Fixed

- **Encryption:** Channel keys are delivered in batches.
- **Encryption:** Key distribution can be cancelled while it waits to retry.
- **Encryption:** Key recovery resumes after a repeated rotation.

### Security

- **Sign-in:** Backing out of a social sign-in no longer costs a later password sign-up its encryption keys.
- **Sign-in:** Two-step verification codes are compared in constant time.
- **Encryption:** The server refuses key distribution into a revoked epoch.
- **Encryption:** Manual channel rotation is fenced to the current credentials.
- **Encryption:** A channel's active key epoch survives concurrent writes.
- **Sign-in:** Signing out other sessions keeps the one you are using.
- **Presence:** Rich Presence suppression fails closed.
- **Presence:** Returning from invisible does not replay a stale suppression.
- **Sign-in:** Static-analysis findings in authentication code are resolved.

## [0.2.37] - 2026-07-27

A stolen or superseded token can no longer outlive the reset meant to end it,
and a disabled account is disconnected at once. Recovery keys use P-384.

**8 Total Changes** | 3 Fixed | 5 Security

### Fixed

- **Desktop:** Electron 43.1.1 fixes crashes when the app archive is replaced while Concord runs.
- **Encryption:** The app bounds how many key requests it sends at once.
- **Messages:** Bulk message removal records its progress, so an interrupted purge can finish.

### Security

- **Sign-in:** Linking a social account is bound to the password you just verified.
- **Sign-in:** Token-theft revocation and session creation cannot interleave.
- **Sign-in:** A disabled account is disconnected from an open connection.
- **Encryption:** Recovery keys use P-384, and weaker curves are rejected.
- **Presence:** Going invisible or offline clears activity that was already published.

## [0.2.36] - 2026-07-26

Desktop actions that touch your camera, clipboard, updates, or permissions
verify that the request came from Concord Voice itself.

**1 Total Changes** | 1 Security

### Security

- **Desktop:** Screen capture, clipboard, update, permission, lifecycle, and developer controls act only for a permitted Concord frame.

## [0.2.35] - 2026-07-25

Windows updates install when you click Restart.

**1 Total Changes** | 1 Fixed

### Fixed

- **Desktop:** Windows updates install from the Restart button, and leftover installer files are cleaned up later.

## [0.2.34] - 2026-07-25

This release carries everything from 0.2.33, which was never published. Google
and Apple sign-in are bound to the current account state, and the "can't reach
Concord servers" warning clears itself.

**4 Total Changes** | 1 Fixed | 3 Security

### Fixed

- **Desktop:** The "can't reach Concord servers" warning clears itself once the app connects.

### Security

- **Sign-in:** Social sign-in is bound to your current account state.
- **Dependencies:** The desktop router update closes GHSA-QWWW-VCR4-C8H2.
- **Encryption:** Key self-healing checks the recipient's current key version.

## [0.2.33] - 2026-07-24

This version was never published, and its contents shipped in 0.2.34.
Sessions, encryption keys, and password changes are tied to the exact
credentials that authorized them.

**14 Total Changes** | 6 Fixed | 8 Security

### Fixed

- **Encryption:** Key recovery stays reachable after a password sign-in.
- **Presence:** Custom Status visibility settles consistently.
- **Presence:** Rich Presence cleanup resumes after an interruption.
- **Voice:** Private calls appear only once both sides are admitted.
- **Presence:** Hidden presence stays hidden across servers.
- **Sign-in:** Sign-in and voice lifecycle code is simpler, with no behavior change.

### Security

- **Sign-in:** Two-step verification during a social sign-in completes inside the protected process.
- **Encryption:** Encryption keys are committed by the sign-in that created them.
- **Sign-in:** A password change that succeeded is never reported as a failure.
- **Encryption:** Key delivery checks the recipient against a concurrent key reset.
- **Sign-in:** A slow rejected request cannot end a rotated session.
- **Sign-in:** Grace-period session refresh follows an exact token lineage.
- **Sign-in:** Every session is bound to the credentials that authorized it.
- **Encryption:** A password change re-encrypts every encrypted setting in one transaction.

---

## [0.2.32] - 2026-07-22

Sign-in and session refresh stay with the account and server you chose, even
when two sign-ins overlap or you switch servers.

**2 Total Changes** | 1 Changed | 1 Fixed

### Changed

- **Dependencies:** mediasoup, Vite, the icon set, and the media server's request parser moved to current releases.

### Fixed

- **Sign-in:** Sign-in and refresh stay with the right account and server, and a superseded sign-in clears its keys from memory.

## [0.2.31] - 2026-07-22

Groundwork for showing what your friends are doing. The server can publish
voice and call activity, and the desktop validates it without showing it yet.

**1 Total Changes** | 1 Changed

### Changed

- **Presence:** The server can publish authorized voice and call activity, and the desktop validates the new format.

## [0.2.30] - 2026-07-18

Reconnecting after a network drop restores what changed while you were away.
Moderators can remove a member's messages when they kick or ban them, and Apple
sign-in works on desktop.

**8 Total Changes** | 3 Added | 2 Fixed | 3 Security

### Added

- **Servers:** Kicking or banning a member can also purge their messages in that server.
- **Operations:** Operators see aggregate account activity in an Admin workspace, as counts only.
- **Operations:** Operators can rotate the two-step verification encryption key safely.

### Fixed

- **Chat:** Reconnecting refreshes your memberships and back-fills the open conversation, and keeps drafts and queued messages.
- **Sign-in:** Apple sign-in completes on desktop through the hosted bridge.

### Security

- **Encryption:** A password change that loses your encryption keys signs you out.
- **Encryption:** A sign-in that has ended cannot clear the keys of the one that replaced it.
- **Server:** Security headers are sent once, from one place.

## [0.2.29] - 2026-07-15

Moderators can remove a member's messages in bulk. The codec in Settings is the
one in use, and H.264 calls are encrypted per access unit. Operators get an
Admin console.

**8 Total Changes** | 3 Added | 3 Fixed | 2 Security

### Added

- **Servers:** Moderators can delete a member's messages across a channel, server, or conversation. The audit record keeps counts only.
- **Operations:** The Admin Portal console shows instance health and aggregate metrics.
- **Presence:** Rich Presence category settings persist across restarts.

### Fixed

- **Video:** The codec shown in Settings is the one in use.
- **Video:** H.264 video is encrypted per access unit, and HDR target selection is corrected.
- **Operations:** Admin metrics stay in bounds, are labelled, and include host telemetry.

### Security

- **Operations:** Desktop release signing is isolated from pull requests.
- **Presence:** The server authorizes voice activity before it publishes it.

## [0.2.28] - 2026-07-14

DM call previews say what happened, and Custom Status counts characters the way
you would.

**3 Total Changes** | 1 Changed | 2 Fixed

### Changed

- **Dependencies:** Desktop build and test tooling was refreshed.

### Fixed

- **Messages:** A DM call entry describes its outcome.
- **Presence:** Custom Status counts characters, not bytes.

## [0.2.27] - 2026-07-14

Activity History arrives: an opt-in, self-only record of your own voice and
call time. A call that drops to software encoding re-selects its codec.

**7 Total Changes** | 2 Added | 4 Fixed | 1 Security

### Added

- **Privacy:** Activity History is an opt-in, self-only record of your voice and call intervals, pruned on your schedule.
- **Operations:** A read-only admin metrics API serves aggregate counts.

### Fixed

- **Video:** Video re-selects a hardware-capable codec mid-call after a software fallback.
- **Chat:** A late layout change no longer scrolls you away from the newest message.
- **Messages:** Edited encrypted messages display correctly.
- **Desktop:** The outgoing-call and What's New dialogs no longer appear behind other windows.

### Security

- **Desktop:** The updater's parser no longer spends excessive CPU on crafted recovery-feed YAML.

## [0.2.26] - 2026-07-12

Everyone can share their screen at once, each stream has its own volume and
mute, and viewers get the resolution their window needs. Codec selection checks
the live call for hardware encoding.

**19 Total Changes** | 4 Added | 2 Changed | 8 Fixed | 5 Security

### Added

- **Screen share:** Everyone can share at once, and each viewer gets the quality that fits their window.
- **Screen share:** Each shared screen has its own volume and mute.
- **Voice:** A voice tile view fills the available space.
- **Presence:** You can hide your Custom Status from specific people.

### Changed

- **Billing:** Subscriptions return to the free tier on schedule when a plan period ends.
- **Settings:** Settings ▸ Sounds and Notifications is now Notifications.

### Fixed

- **Video:** Hardware video encoding is confirmed from the live call.
- **Voice:** Joining during a brief server blip retries instead of failing.
- **Voice:** Picture-in-picture windows close promptly when a call ends.
- **Screen share:** System audio is captured only for whole-screen shares.
- **Screen share:** Tuning into a second screen keeps the focused one.
- **Screen share:** Quality demand no longer flaps for AV1 and VP9 shares.
- **Desktop:** Interface and app update states show separately.
- **Notifications:** Follow-up fixes from the desktop notification work.

### Security

- **Voice:** The media server checks permission to speak or share.
- **Voice:** An empty server-supplied voice identity fails closed.
- **Video:** Channel video limits follow the server's plan, not the owner's.
- **Screen share:** Screen-share resolution and frame rate follow your plan as a combined pixel rate.
- **Server:** Shutdown drains in-flight requests before closing live connections.

## [0.2.25] - 2026-07-09

A full audit of channel and message permissions checks visibility on every path
that can reveal it. The composer adds emoji autocomplete and picker shortcuts.

**18 Total Changes** | 4 Added | 4 Fixed | 10 Security

### Added

- **Messages:** Type a `:shortcode:` for inline emoji suggestions.
- **Messages:** Ctrl+E opens emoji and Ctrl+G opens GIFs from the composer.
- **Screen share:** You choose which shared screens to watch.
- **Bug reports:** A preview shows the redacted diagnostic log before you send a report.

### Fixed

- **Messages:** The composer no longer jumps when a picker first opens.
- **Messages:** Emoji-only messages render large again.
- **Settings:** Privacy and Security settings no longer sign you out.
- **Accessibility:** Dialogs trap focus and announce themselves to screen readers.

### Security

- **Servers:** Channel metadata, unread counts, attachments, keys, broadcasts, and voice lists require permission to view the channel.
- **Servers:** Kicked or banned members are evicted from live connections.
- **Privacy:** Group conversation creation respects your privacy settings.
- **Servers:** The database refuses to bind a channel to another server's category.
- **Sign-in:** SSO state and one-time tokens are consumed atomically, and attestation tokens are bound to their account.
- **Voice:** Voice display names and avatars come from the server.
- **Operations:** Admin enrollment links keep their token out of the URL.
- **Server:** The server refuses a wildcard origin in production.
- **Dependencies:** The tar library update blocks malicious archives in build tooling.
- **Sign-in:** Running out of verification attempts returns the same shape as other failures.

## [0.2.24] - 2026-07-05

Username search, the friend-request sound, password-reset email, and the
verification-code window work again. Voice adds a stage that shows who is
talking.

**11 Total Changes** | 2 Added | 8 Fixed | 1 Security

### Added

- **Voice:** A voice stage puts the active speaker in front and marks the call as encrypted.
- **Sign-in:** The sign-in screen has a show-password toggle.

### Fixed

- **Encryption:** The app notices when your keychain cannot save your keys for next launch.
- **Friends:** Searching for a user by name works again.
- **Friends:** Friend requests make a sound again.
- **Sign-in:** Password-reset email is delivered.
- **Sign-in:** The email verification code window is long enough to use.
- **Voice:** One-to-one calls show a single bottom bar.
- **Settings:** Settings and help overlays can be closed without the keyboard.
- **GIFs:** A GIF that fails to load no longer signs you out.

### Security

- **Server:** The server refuses test-mode configuration in production.

---

## [0.2.23] - 2026-07-04

You can see and manage your subscription in the app, and call quality, upload
limits, and server features follow your plan. When bandwidth is tight, Concord
drops webcam first, then screen share, so voice stays clear.

**6 Total Changes** | 4 Added | 1 Changed | 1 Fixed

### Added

- **Billing:** Settings ▸ Subscription shows your plan and status and redeems codes.
- **Billing:** Call quality, per-room camera and screen-share limits, and server features follow your plan. Webcam drops first, then screen share, before audio.
- **Billing:** Server-creation and search-depth limits scale with your subscription.
- **GIFs:** The GIF pipeline keeps animation and guards against oversized files.

### Changed

- **Operations:** Release workflows use tighter least privilege, and the build toolchain and dependencies were refreshed.

### Fixed

- **Sign-in:** SSO sign-up gets a 15-minute token window and recovers if it expires.

## [0.2.22] - 2026-07-02

After you update, the next launch tells you what changed. This file became the
public record of every release, and CI refuses a version without an entry.

**2 Total Changes** | 2 Added

### Added

- **Desktop:** A "What's new" dialog covers every version since the one you had. It shows once and works offline.
- **Desktop:** This changelog is the canonical public record, and CI refuses a release without its entry.

## [0.2.21] - 2026-07-02

Windows verifies an update's signature before it installs, and public releases
carry build provenance you can check. Pinning works again, and scroll stays
where you left it.

**12 Total Changes** | 1 Added | 1 Changed | 7 Fixed | 3 Security

### Added

- **Billing:** Avatar, banner, and server-icon size limits scale with your plan.

### Changed

- **Operations:** Tooling, CI, and documentation upkeep.

### Fixed

- **Messages:** Server message pinning works again, and pinned media stays in its panel.
- **Messages:** Switching channels quickly no longer snaps you to a stale position.
- **Notifications:** DM and notification previews say "Photo" instead of a placeholder.
- **Messages:** The composer menu renders above the placeholder text.
- **Server:** The client no longer makes permission checks it was never entitled to make.
- **Desktop:** UI and app update checks share one path.
- **Sign-in:** The suggested identity at sign-up no longer carries a stray "@concordvoice.chat" suffix.

### Security

- **Desktop:** Windows checks the code signature of each update against the expected publisher.
- **Operations:** Every publicly mirrored desktop release asset carries verifiable build provenance.
- **Operations:** A misconfigured attestation deployment fails loudly.

## [0.2.20] - 2026-06-30

Self-hosted servers get every feature with no plan limits. The licence, privacy
policy, and terms are one click away in About.

**2 Total Changes** | 2 Added

### Added

- **Self-hosting:** Self-hosted servers unlock every premium entitlement with no subscription.
- **Settings:** Settings ▸ About shows the licence, terms, and other notices.

## [0.2.19] - 2026-06-30

You can sign in to a Concord Voice server that you run yourself. A client stuck
on an old interface can recover without a reinstall.

**3 Total Changes** | 1 Added | 1 Changed | 1 Fixed

### Added

- **Self-hosting:** Desktop sign-in can route to an operator-run server.

### Changed

- **Operations:** Deployment and tooling upkeep.

### Fixed

- **Desktop:** The app checks the public release feed for updates, so a stranded client can recover.

## [0.2.18] - 2026-06-29

Certificate pinning is back on for connections to Concord Voice servers.

**1 Total Changes** | 1 Security

### Security

- **Desktop:** Certificate pinning for connections to Concord services is restored after it lapsed.

## [0.2.17] - 2026-06-29

You can react to DMs with emoji. The app connects reliably at launch, and a
certificate rotation no longer locks you out.

**4 Total Changes** | 1 Added | 1 Changed | 2 Fixed

### Added

- **Messages:** DMs take emoji reactions.

### Changed

- **Operations:** Tooling, CI, and dependency upkeep.

### Fixed

- **Chat:** A race in the WebSocket sign-in handshake no longer leaves you disconnected after launch.
- **Desktop:** The client trusts the rotated API certificate pin.

## [0.2.16] - 2026-06-29

You choose how much a notification shows on screen. Custom themes are free, and
opening DMs no longer interrupts your call.

**5 Total Changes** | 2 Added | 1 Changed | 2 Fixed

### Added

- **Notifications:** A setting controls how much message content desktop notifications show.
- **Appearance:** Custom themes are free for everyone.

### Changed

- **Operations:** A legacy automation credential was retired, and the supply-chain list refresh is guarded.

### Fixed

- **Voice:** Call audio keeps playing when you open DMs.
- **Desktop:** macOS relaunches after an update installs.

## [0.2.15] - 2026-06-29

Moderators can time a member out instead of removing them. Passkey registration
is stricter, and erasing your account revokes the session that asked for it.

**5 Total Changes** | 2 Added | 1 Changed | 2 Security

### Added

- **Servers:** Moderators can time a member out for a set duration.
- **Desktop:** On first launch outside /Applications, macOS offers to move the app there.

### Changed

- **Operations:** Tooling, CI, and deployment upkeep.

### Security

- **Sign-in:** Passkey registration requires platform-authenticator options.
- **Accounts:** Erasing your account revokes your current access token.

## [0.2.14] - 2026-06-28

A voice channel can set one audio quality for everyone in it. Usernames are
case-insensitive everywhere, and self-hosted certificate failures are reported.

**10 Total Changes** | 3 Added | 2 Changed | 3 Fixed | 2 Security

### Added

- **Voice:** A channel can set one audio quality tier for every member, bounded by the server's tier.
- **Video:** Settings ▸ Audio & Video has SVC and simulcast toggles.
- **Self-hosting:** One script provisions a self-signed, Let's Encrypt, or bring-your-own certificate.

### Changed

- **Settings:** My Profile moved into Settings ▸ Account, and the SSO controls were polished.
- **Operations:** Tooling, CI, and dependency upkeep.

### Fixed

- **Accounts:** Usernames are case-insensitive everywhere.
- **Notifications:** Notification settings match the macOS permission state.
- **Self-hosting:** Self-hosted certificate provisioning errors are reported.

### Security

- **Sign-in:** Email MFA setup is hardened for production.
- **Desktop:** The cached-UI origin is on the server allowlists before the cache activates.

## [0.2.13] - 2026-06-28

Invite bubbles name the person who invited you. Linux updates are verified
before they install, and the signed offline interface cache is live.

**5 Total Changes** | 1 Changed | 2 Fixed | 2 Security

### Changed

- **Operations:** A retired sign-in configuration value was removed.

### Fixed

- **Invites:** Invite bubbles name the person who sent the invite.
- **Settings:** System Permissions settings are easier to find and understand.

### Security

- **Desktop:** Linux AppImage, deb, and rpm updates carry Ed25519 signatures, and an unverified update does not install.
- **Desktop:** The signed offline UI cache is live and verified before it runs.

## [0.2.12] - 2026-06-27

Your camera and screen share stay live when the call switches video quality
layers.

**3 Total Changes** | 1 Changed | 2 Fixed

### Changed

- **Operations:** Engineering write-ups, storage-config groundwork, and static-analysis cleanup.

### Fixed

- **Video:** Camera and screen share stay live when video layers switch.
- **Accounts:** Age-verification status survives a reload.

## [0.2.11] - 2026-06-26

Hardware video encoding is offered only when your graphics hardware supports
it.

**1 Total Changes** | 1 Fixed

### Fixed

- **Video:** Hardware encoding uses only codecs your GPU accelerates.

## [0.2.10] - 2026-06-26

You can run your own Concord Voice instance. AV1 video no longer goes black in
encrypted calls.

**3 Total Changes** | 1 Added | 1 Changed | 1 Fixed

### Added

- **Self-hosting:** A guided installer sets up a self-hosted instance, and `concord-selfhost` manages it.

### Changed

- **Operations:** Supply-chain indicators refreshed, and macOS builds retry a transient DMG flake.

### Fixed

- **Video:** AV1 video decrypts reliably in encrypted calls (frame crypto v4).

## [0.2.9] - 2026-06-24

Video no longer goes black after a key rotates mid-call, and the offline
interface cache is signed.

**6 Total Changes** | 2 Added | 1 Changed | 2 Fixed | 1 Security

### Added

- **Desktop:** The app keeps a signed last-known-good copy of the interface.
- **Server:** `GET /api/v1/server/capabilities` tells a client what a server supports.

### Changed

- **Operations:** Tooling and workflow upkeep.

### Fixed

- **Video:** Video frames carry their key version, so video survives a mid-call key rotation.
- **Operations:** Public release mirroring triggers only on real releases.

### Security

- **Desktop:** Packaged builds accept only the official API address or a verified self-hosted server.

## [0.2.8] - 2026-06-24

Audio devices switch mid-call, DM calls open with sound, and encrypted video
keeps up with layered streams. Older DM attachments decrypt again.

**9 Total Changes** | 2 Changed | 7 Fixed

### Changed

- **Invites:** The invite landing page is restyled.
- **Operations:** Tooling, CI, and test upkeep, including automated contrast checks.

### Fixed

- **Desktop:** The client retries the latest UI after a cold-start network problem, and session-only sign-in survives a reload.
- **Video:** Encrypted video stays in sync with layered and simulcast streams.
- **Voice:** You can switch audio devices mid-call.
- **Voice:** DM calls open the call view and play remote audio.
- **Messages:** Older DM attachments decrypt correctly.
- **Presence:** Your DM list shows accurate online status.
- **Messages:** Enlarged images are centered.

## [0.2.7] - 2026-06-23

> Versions 0.2.4 to 0.2.6 never shipped. Everything they carried ships here.

Images open in a lightbox, and invite links can be public. Camera quality adapts
in group calls, and video recovers once a key catches up.

**10 Total Changes** | 3 Added | 1 Changed | 5 Fixed | 1 Security

### Added

- **Messages:** Click an image to see it full size, zoom, and save it.
- **Invites:** Public invite links open a landing page.
- **Video:** SVC-first camera layers send each viewer the resolution they need.

### Changed

- **Dependencies:** Electron 42.4.1, mediasoup 3.20.9, and release-pipeline upkeep.

### Fixed

- **Video:** Encrypted video recovers after a key epoch desync.
- **Voice:** Output device changes apply mid-call.
- **Messages:** A batch of chat and UI fixes for DMs, mentions, lists, code, menus, and voice frames.
- **Desktop:** Linux packages launch reliably, show menu icons, and keep RPM sandbox permissions.
- **Server:** Media uploads return "service unavailable" when storage is disabled.

### Security

- **Servers:** Channel permissions are enforced on the WebSocket path as well as REST.

## [0.2.3] - 2026-06-22

The composer shows how many characters you have left and keeps what you typed
when you go over.

**2 Total Changes** | 1 Added | 1 Changed

### Added

- **Messages:** A live character counter flags over-limit text before you send.

### Changed

- **Operations:** Build, scanning, and documentation upkeep.

## [0.2.2] - 2026-06-22

Premium features show what they unlock and can be redeemed with a code. Voice
crackle is fixed, and the app reconnects after a server deploy.

**8 Total Changes** | 5 Added | 1 Changed | 2 Fixed

### Added

- **Billing:** Premium lock badges, redemption codes, and server-enforced quality limits per plan.
- **Feedback:** A public Known Issues list is linked from the feedback dialog.
- **Desktop:** Settings ▸ About has a "Load latest UI" button, and launch retries it.
- **Operations:** Operator console sign-in needs a password and a WebAuthn hardware key.
- **GIFs:** New accounts load KLIPY GIFs automatically. Existing choices do not change.

### Changed

- **Operations:** Feedback triage, mirror fixes, and opt-in audio diagnostics.

### Fixed

- **Voice:** Small calls no longer clip or garble the first words of speech.
- **Chat:** The app reconnects after a server deploy, and a sign-in blip keeps Remember Me.

---

## [0.2.1] - 2026-06-20

> Patch release. v0.2.0-Beta shipped the macOS client as a `.zip` only. This
> release attaches the signed and notarized `.dmg` installer as well.

Friend categories arrive: group your DMs with your own emoji and colors. The
grouping is encrypted with your key, so the server cannot read it.

**2 Total Changes** | 1 Added | 1 Fixed

### Added

- **Friends:** Friend categories group your DMs, encrypted with your key.

### Fixed

- **Desktop:** Releases attach the macOS `.dmg` installer as `ConcordVoice-<version>-macos-<arch>.dmg`. Auto-update stays on the `.zip`.

## [0.2.0-Beta] - 2026-06-20

> Release-level rollup of Phase 2A and Phase 2B. Per-revision detail is in the
> 0.1.12 to 0.1.18 entries below.

The Beta release. Sign in with Google or Apple, add two-step verification or a
passkey, and recover a locked account. Roles and permissions arrive with GIFs,
server moderation, and end-to-end encrypted attachments.

**101 Total Changes** | 38 Added | 8 Changed | 3 Removed | 30 Fixed | 22 Security

### Added

- **Sign-in:** Google sign-in and registration.
- **Sign-in:** Sign in with Apple, aware of Apple's private relay.
- **Security:** TOTP, WebAuthn/FIDO2, backup codes, recovery circles, and trusted devices.
- **Accounts:** `POST /api/v1/privacy/erase-account` erases an account across every linked table.
- **Accounts:** Zero-knowledge account key recovery.
- **Servers:** Role- and server-based access control with audit logging.
- **Server:** User images moved from PostgreSQL to S3-compatible storage with two-tier access.
- **Servers:** Server ownership transfer with MFA, email confirmation, and reversal tokens.
- **Sign-in:** Email verification on registration, later sent through Resend.
- **Sign-in:** Registrations stay pending for 15 minutes until verified.
- **Messages:** Reactions, replies, pinning, encrypted search, attachments, drafts, notifications, shortcuts, group DMs, and extended Markdown.
- **GIFs:** KLIPY GIF search and picker through a privacy proxy.
- **Voice:** Server-enforced mute and deafen, applied at the media server.
- **Notifications:** @mention routing in encrypted channels without decryption.
- **Encryption:** DM key-epoch enforcement and revocation.
- **Encryption:** Channel keys rotate when a member is removed.
- **Desktop:** Auto-updater with rollback, splash screen, progress, and error states.
- **Desktop:** Server-proxied desktop updates with no per-client telemetry.
- **Desktop:** Hot-update SPA pipeline with a bundle-hash deploy contract.
- **Desktop:** The desktop falls back to the bundled SPA when a hot update fails.
- **Chat:** WebSocket reconnect race fix and subscribe-barrier protocol.
- **Self-hosting:** Self-hosted coturn STUN/TURN.
- **Server:** Public media proxy for public assets.
- **Sign-in:** Token theft detection binds sessions to machine ID and IP.
- **Sign-in:** The main process refreshes the access token before expiry.
- **Appearance:** Per-user profile theming, profile cards, and avatar theming.
- **Appearance:** Image crop editors for profile and server images.
- **Accounts:** Username rules, period support, and a yearly change cooldown.
- **Desktop:** OS-level permission management.
- **Friends:** Friend requests with accept and decline.
- **Notifications:** Notification sounds, per-category volumes, and looping DM call sounds.
- **Desktop:** A Developer Mode setting enables DevTools in Alpha and Beta builds.
- **Desktop:** macOS builds are signed and notarized.
- **Desktop:** Windows `Setup.exe` is signed.
- **Operations:** Docker network segmentation, request-ID propagation, and Redis auth.
- **Operations:** GitHub Actions CI with tests, coverage, SonarQube, and Semgrep.
- **Operations:** A supply-chain IOC scanner.
- **Operations:** An AI-generated code governance framework.

### Changed

- **Dependencies:** React 19.2.4, react-router-dom 7.13.1, Zustand 5.0.12, ESLint 10 flat config, and Vite 8.0.0.
- **Dependencies:** Go 1.26.1 with `govulncheck`.
- **Dependencies:** Electron 41, mediasoup 3.19.18, mediasoup-client 3.18.7, and TypeScript 6.0.2.
- **Encryption:** The client derives the password key with Argon2id instead of PBKDF2.
- **Operations:** macOS notarization uses an App Store Connect API key.
- **Operations:** Lower cognitive complexity across Go handlers and TypeScript code.
- **Operations:** Documentation audit for the Beta release gate.
- **Operations:** MCP server config split into per-host files.

### Removed

- **Operations:** Sentry telemetry removed from all three services, and its MCP config removed.
- **Operations:** The unused Postgres MCP server config.
- **Desktop:** The deprecated `WebSocketMessage` type alias. `WebSocketEvent` is the canonical name.

### Fixed

- **Encryption:** Channel-key OperationError reports a structured diagnostic.
- **Encryption:** WebRTC BUNDLE no longer misroutes encrypted voice and video between consumers.
- **Encryption:** Media encryption moved from Insertable Streams to RTCRtpScriptTransform.
- **Encryption:** A session-scoped cache stops the key request flood.
- **Encryption:** Frame decryption recovers, and CSK rotation is hardened.
- **Encryption:** DM key epochs, presence sync, and key distribution are fixed.
- **Messages:** Replied-to messages decrypt on REST fetch.
- **Messages:** DM threads update their preview and order in real time.
- **Messages:** Message editing works in encrypted channels and DMs.
- **Voice:** Black-screen recovery, codec selection, screen-share audio, and audio that persists across navigation.
- **Server:** Hub goroutine races and flaky voice tests are resolved.
- **Chat:** A lost connection reconnects the WebSocket instead of reloading the page.
- **Presence:** You no longer show as Offline while connected.
- **Server:** The Postgres "invalid length of startup packet" flood is fixed.
- **Voice:** The PiP window loads the SPA route.
- **Voice:** PiP signaling and local user identification are fixed.
- **Desktop:** Login on the bundled SPA fallback uses the `app://` scheme.
- **Sign-in:** The `/mfa/verify` response parses `access_token`.
- **Desktop:** The updater trust path is hardened on macOS, Windows, and CI.
- **Operations:** CI verifies ASAR integrity.
- **Desktop:** The preload script is bundled with esbuild for the sandbox.
- **Operations:** Docker Compose passes the MFA encryption key.
- **GIFs:** The KLIPY media proxy no longer returns 401, and its routes sit under `/gifs`.
- **GIFs:** KLIPY GIFs render on every decrypt path.
- **Operations:** NATS config and coturn TLS and external IP are fixed.
- **Desktop:** Escape closes only the top modal.
- **Messages:** Server role styling no longer affects DMs.
- **Accessibility:** Semantic HTML, keyboard navigation, and ARIA pass.
- **Messages:** A Markdown syntax help dialog and help icon.
- **Operations:** Beacon verification, the `/spa/` nginx route, and a defensive sentinel.

### Security

- **Desktop:** Main-process logs no longer pass raw `Error` objects.
- **Sign-in:** Ten token-suffix leaks are removed from the token manager.
- **Desktop:** Passive navigation allows only `https:`. Clicked links still allow `http:`, `https:`, and `mailto:`.
- **Desktop:** The updater pins the TLS certificate for `api.concordvoice.chat`.
- **Operations:** nginx closes H2C smuggling and Host header injection.
- **Operations:** A GitHub Actions shell injection is resolved.
- **Server:** CORS rejects null or empty origins and validates custom headers.
- **Server:** Hardcoded dev credentials are removed, and production guards are added.
- **Operations:** Infrastructure resists brute-force probes and vulnerability scanners.
- **Dependencies:** npm overrides fix transitive vulnerabilities.

**Known issues** (not counted as changes):

- The "No Internet" Retry button can stay stuck after the network returns. Quit and relaunch.
- Chat Markdown has heading size, code block, and spacing errors.
- The Pinned Messages panel drops media and shows GIFs as raw text.
- The GIF picker can hit a rate limit while you scroll.
- The Member List "+ Add Role" dropdown is clipped inside its card.
- The profile editor can lose unsaved edits when your user record refreshes.

**Upgrading from v0.1.0-Alpha** (not counted as changes):

- No end-user breaking changes. New features are additive.
- Migrations 000054 to 000061 apply on first server run. Migration 000059 makes `refresh_tokens.user_id` cascade on delete.
- The client re-wraps legacy PBKDF2 keys with Argon2id at next login (migration 000034).
- Passive navigation allows only `https:`.
- A password change revokes refresh tokens, so other devices sign out at their next refresh.
- Self-hosted operators who want Apple sign-in must set the Apple credentials in the control-plane `.env`.

---

## [0.1.41] - 2026-05-20

Windows and Linux get native window buttons, and the app gets a branded title
bar. You can now choose what the close and minimize buttons do.

**7 Total Changes** | 4 Added | 3 Changed

### Added

- **Desktop:** Windows and Linux show native close, minimize, and maximize buttons. macOS keeps its traffic lights.
- **Desktop:** A branded title bar shows the app name, the version, and the active SPA build, and updates when the SPA updates.
- **Desktop:** The window remembers its size, position, and maximized state, and checks the saved position against your current displays.
- **Desktop:** Settings ▸ Appearance ▸ Client Behavior lets you send the close and minimize buttons to the tray, the taskbar or dock, or quit. Invalid combinations are greyed out with a reason. The tray icon itself ships with.

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
- **Notifications:** You can mute messages and voice separately for each server and DM.
- **Messages:** The server no longer sends an `is_encrypted` field, because every room is encrypted. WebSocket messages without `key_version` 1 or higher are refused with close code 4400.
- **Messages:** The desktop no longer reads `is_encrypted`.
- **Voice:** The media server drops `is_encrypted` and documents that SRTP is always on.

### Fixed

- **Desktop:** The release workflow no longer skips publishing on a push to main.
- **Desktop:** The first WebSocket connection drop no longer logs noise, and the auth ticket no longer shows in the console.

---

## [0.1.18] - 2026-04-09

The desktop reports crashes and errors, and scrubs private data before it
sends them.

**3 Total Changes** | 2 Added | 1 Changed

### Added

- **Desktop:** Error tracking for the main and renderer processes. It fails closed and drops key material, personal data, and console logs before it sends a report.
- **Development:** A documentation-drift reviewer for pull requests.

### Changed

- **Development:** The project guides and architecture docs describe the current state of the app.

---

## [0.1.17] - 2026-04-09

A quality pass fixes GIF routes and hardens the voice relay servers.

**6 Total Changes** | 1 Added | 1 Changed | 4 Fixed

### Added

- **Development:** A script keeps the internal project counts current.

### Changed

- **Development:** Tool server settings use one naming scheme.

### Fixed

- **GIFs:** GIF requests reach the right server route.
- **Development:** Review comments from an earlier pull request are addressed.
- **Voice:** The voice relay server uses its own TLS certificate, covers its hostname, and renews it automatically.
- **Operations:** The message bus and voice relay settings are correct for a single server.

---

## [0.1.16] - 2026-04-08

Public images load without sign-in, and a quality pass fixes many small bugs.

**3 Total Changes** | 1 Added | 2 Fixed

### Added

- **Servers:** Public images load without sign-in.

### Fixed

- **Appearance:** Avatars keep a fixed 40 by 40 pixel size, so the layout no longer shifts.
- **Desktop:** A broad sweep fixes app, server, and infrastructure bugs.

---

## [0.1.15] - 2026-04-07

GIF search arrives, and the server fetches GIFs for you so the GIF provider
never sees your address.

**9 Total Changes** | 2 Added | 1 Changed | 6 Fixed

### Added

- **GIFs:** GIF search and a GIF picker. The server fetches GIFs on your behalf, and the About screen credits the provider.
- **Desktop:** Alpha builds have a Developer Mode switch that opens DevTools.

### Changed

- **Dependencies:** Routine updates to build, test, and sign-in libraries.

### Fixed

- **Desktop:** Windows builds work again.
- **Messages:** Chat messages, GIFs, image crops, and SPA updates display correctly.
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

- **Voice:** Voice settings, the voice service, encryption helpers, and codec choice are split into smaller parts.

### Fixed

- **Servers:** Right-click kick, ban, and mute follow your server role.

---

## [0.1.13] - 2026-04-04

Chat gets reactions, replies, pins, search, attachments, and group DMs.
Servers can mute and deafen members, and the voice server enforces it.

**34 Total Changes** | 15 Added | 7 Changed | 10 Fixed | 2 Security

### Added

- **Voice:** Server mute and deafen, enforced by the voice server.
- **Messages:** Emoji reactions that sync live.
- **Messages:** Replies that quote the original message.
- **Messages:** Pinned messages with a pin list for each channel.
- **Messages:** Encrypted search that decrypts and searches history on your device.
- **Messages:** File and image attachments with preview and download.
- **Messages:** Group DMs with admin controls.
- **Messages:** Unsent drafts are saved for each channel.
- **Notifications:** Desktop notifications for mentions and new DMs.
- **Desktop:** Configurable keyboard shortcuts with a help overlay.
- **Desktop:** One right-click menu across the app, with copy and role assignment.
- **Desktop:** The SPA can update without a new desktop release.
- **Development:** Shared chat logic for fetch, paging, decryption, and send.
- **Operations:** Services run on separate networks, requests carry an ID, and Redis requires auth.
- **Development:** An AI-generated code policy, code owners, and static analysis.

### Changed

- **Messages:** The message component is split into smaller parts.
- **Messages:** The DM list updates live with the latest message.
- **Dependencies:** TypeScript 6, ESLint 10, mediasoup, Electron, and other updates.
- **Accounts:** Account email uses a new provider with branded templates.
- **Development:** CI adds static analysis, quality gates, test sharding, and caching.
- **Development:** Server and app code is simpler to read.
- **Desktop:** Modals share one set of panels.

### Fixed

- **Messages:** Replied-to messages decrypt when history loads, not only live.
- **Messages:** Server role colors no longer appear in DMs.
- **Development:** WebSocket and voice tests no longer depend on timing.
- **Servers:** Assigning a role from the right-click menu reports failures.
- **Desktop:** Escape closes only the top modal.
- **Video:** Video fills the voice area.
- **Desktop:** A lost connection reconnects without a page reload.
- **Accessibility:** Semantic markup, keyboard navigation, and screen-reader labels across the app.
- **Development:** Safe database query warnings are marked as reviewed.
- **Voice:** The media server reaches Redis.

### Security

- **Operations:** The web server blocks request smuggling and Host header injection.
- **Development:** A CI workflow no longer allows shell injection.

---

## [0.1.12] - 2026-03-27

Accounts get two-factor sign-in, security keys, and email verification.
Servers get roles and permissions, and the desktop updates itself.

**36 Total Changes** | 16 Added | 6 Changed | 10 Fixed | 4 Security

### Added

- **Accounts:** Two-factor sign-in with authenticator apps, security keys, backup codes, recovery contacts, and trusted devices.
- **Servers:** Roles and permissions with an audit log.
- **Accounts:** Email verification at sign-up.
- **Servers:** Images are kept in object storage, not the database.
- **Servers:** Ownership transfer with two-factor confirmation, email confirmation, and a way to reverse it.
- **Desktop:** Automatic updates with rollback, a splash screen, and progress.
- **Accounts:** A stolen sign-in token is revoked when it appears on another device or address.
- **Accounts:** Sign-in tokens refresh before they expire.
- **Messages:** Removing a member rotates the channel key.
- **Messages:** Revoked DM keys are refused on the live connection.
- **Messages:** Mentions reach people without the server reading the message.
- **Desktop:** The app asks for OS permissions and respects them.
- **Voice:** A self-hosted relay helps calls connect through strict networks.
- **Development:** CI runs tests, coverage, and code quality checks in parallel.
- **Development:** Many new tests toward the 80% coverage gate.
- **Desktop:** Install and update steps are logged to a file.

### Changed

- **Dependencies:** React 19, React Router 7, Zustand 5, Vite 8, Go 1.26, Electron 41, and mediasoup 3.19.
- **Development:** ESLint 10 flat config and Redis client 5.
- **Messages:** Encryption keys are derived with Argon2id instead of PBKDF2.
- **Messages:** Shared message fetch, decrypt, and paging logic.
- **Desktop:** The app is named "Concord Voice" on every platform, with the correct version, icon, and publisher.
- **Desktop:** The updater no longer shares files with other Electron apps.

### Fixed

- **Voice:** Encrypted voice and video no longer mix up streams.
- **Voice:** The microphone works in encrypted voice on macOS.
- **Video:** Codec choice and screen-share sound work.
- **Voice:** Audio continues when you move to another view.
- **Video:** Black screens and missing audio in calls are fixed.
- **Messages:** Edits save.
- **Appearance:** Profiles use the owner's color scheme.
- **Desktop:** Clicking a popover's button again closes it.
- **Screen share:** Screen sharing uses your video settings.
- **Servers:** Changing a role no longer crashes the app.

### Security

- **Operations:** The server refuses empty or null origins and checks custom headers.
- **Operations:** Development credentials are removed from the code, with production guards.
- **Operations:** The infrastructure blocks brute-force scans.
- **Dependencies:** Vulnerable indirect packages are pinned to fixed versions.

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
