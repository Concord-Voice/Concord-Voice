# Changelog

All notable changes to Concord Voice will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

Trusted-device recovery asks you to compare a fingerprint on both devices
before transferring your account key. Voice joins capture the selected
microphone once and explain how to recover if capture fails. Audio and video
device controls now sit with their settings, with app-only microphone level
adjustment when automatic gain is off. Calls and Microphone Test share
microphone processing, with a sample-peak ceiling while automatic gain is on.
The noise gate can now adapt during a call or Microphone Test, or use a fixed
threshold you set yourself.
The desktop also includes a patched
source-map-js dependency. Local camera previews stay
visible during producer swaps, and a remote tile returns when its replacement
attaches. A remote close can briefly clear the tile if it arrives first. Brief
receiver gaps no longer turn off layered camera publishing for the whole room.
Late media from an earlier voice session is closed before it can attach after a
rejoin. Settings sections now start collapsed and remember what you expand for
the rest of the session. Up Arrow in an empty message box edits your last
message. Confirming a purge, turning off purge protection, or clearing a DM's
history now asks only for what your account uses. An account without an
authenticator app or security key no longer sees a code box it cannot fill, a
passkey or security key works in one step, and the prompt opens on the method
you used most recently. Server owners can require MFA for dangerous actions
from Server Settings, and members who need it are told how to set it up.

### Added

- **Purging messages now keeps pinned messages unless you choose to include them** (internal development reference omitted, internal development reference omitted) —
  every purge (channel, server, DM, group, and the purge a kick or ban can run) now has an "Include
  pinned messages" option, off by default. With it off, pinned messages are kept: your own are not deleted, and
  in a DM or group the pinned messages of others stay visible to you instead of being hidden. The
  dialog and the result say which happened. Update the app to see the option; older versions keep
  pinned messages without saying so. **API clients:** the channel, server and DM purge bodies and the
  kick and ban bodies accept `include_pinned` (default `false`), and `GET /server/capabilities` reports
  `features.purgeKeepsPinned: true` on a server that honours it. Clearing a chat's history no longer
  hides its pinned messages either, and unpinning one hides it again.
- **Servers that require MFA for dangerous actions now ask for a code before they do them** (internal development reference omitted). When an owner turns on "Enforce MFA on dangerous actions", a member must confirm with an authenticator code, a backup code or a security key before they edit the server, upload its icon or banner, delete it, delete a channel, shorten a channel's message expiration or apply an expiration window to messages already sent, ban a member, kick a member and purge their messages, purge a channel or server of other people's messages, delete a role, or grant Administrator, Manage Developer Resources or Manage E2EE Keys by creating or editing a role or by setting a channel or category permission override. Creating or editing a role, or setting an override, that grants none of those three is not asked for a code. A refused request changes nothing. After you confirm one action, the same session needs no code for another action that needs the same permission for 10 minutes; deleting a server, turning enforcement off and granting a dangerous permission always ask again. An owner with no authenticator is held to the same limits as any other member who has none, and is told to set one up. A server without the setting behaves as before. **API clients:** send `mfa_code` as a JSON field (multipart for the two image uploads; an optional JSON body on the channel, role and server DELETE routes) only on a retry after a 403 with `mfa_required`. The refusals are 403 `mfa_required`, 403 `Invalid MFA code`, 403 `mfa_enrollment_required`, 429 `step_up_budget_exhausted`, 503 `step_up_budget_unavailable` and 503 `lock_conflict` with `Retry-After: 1`. A request that carries a code counts against the attempt budget even on a server that does not enforce, and while Redis is down a request with a code gets the 503. `GET /api/v1/servers/{id}/permissions` now reports `mfa_restricted: true` when enforcement is withholding a permission from you, and `GET /api/v1/server/capabilities` reports `features.mfaEnforcedDangerousActions`. The server icon, server banner and group DM icon uploads no longer delete the live image when recording a new one fails.
- **Server owners can require MFA for dangerous actions from Server Settings** (internal development reference omitted, internal development reference omitted) — owners and Administrators see a "Require MFA for dangerous actions" switch under Security in General. Turning it on needs your own authenticator app or security key first, and turning it off asks you to confirm with a code or security key. On a server that requires MFA, each dangerous action now asks for a code or security key in a "Confirm it's you" dialog, and it carries out exactly what you had chosen once you confirm. This covers deleting a channel, a server or a role, banning a member, kicking a member and purging their messages, shortening message expiration, saving server settings, uploading a server icon or banner, saving a role or a channel or category override that grants a dangerous permission, and purging a channel or server. A member whose account has no authenticator app or security key sees a notice under the server name and a "Set up verification" item in the server, channel and member menus. Those open Privacy & Security, and "Back to chat" or "Back to Server Settings" returns you when setup is done. Permissions that the server withholds come back without a reload once you finish setup. The role editor marks Administrator, Manage Developer Resources and Manage E2EE Keys as needing MFA on these servers. Desktop builds from before this release do not show these prompts: a moderator on one gets the old generic error until they update.
- **Delete Role asks you to confirm** (internal development reference omitted) — the button used to delete the role on one click. It now names the role and says members who hold it lose its permissions. The role list also keeps your selection when the delete does not go through.
- **Up Arrow edits your last message** (internal development reference omitted) — in an empty message box, Up Arrow opens your most recent message for editing, in channels, direct messages and voice text chat. It keeps its usual cursor behaviour while the box holds text, a reply or an attached file, while an input method is composing, and with Shift, Ctrl, Alt or Cmd held. When your newest message is still sending, failed to send or cannot be read, Up Arrow does nothing rather than opening an older message. Any message edit now opens with the cursor at the end of the text, saving or cancelling it returns focus to the message box you were using, and the edit box has an accessible name. If an edit cannot be saved, the edit box opens again with your text. An edit you leave open closes when you switch channels, and a message that cannot be read offers Delete but not Edit.

### Fixed

- **Verification setup cancels stale returns and confirmations** (internal development reference omitted) — Closing or replacing Settings cancels a pending return from verification setup. A message timer that changes while verification is open now abandons the old confirmation, so it cannot apply a choice made for the previous timer. The staged timer stays available for fresh confirmation unless the new policy already applies it.
- **Setting up verification no longer loses unsaved settings** (internal development reference omitted, internal development reference omitted) — when Server Settings sent you to set up verification from Roles or Members, changes you had made to the server's name, icon or banner were lost without asking. Server Settings now asks first, from every section. In App Settings, **Back to chat** and **Back to Server Settings** now wait until you apply or revert your changes, as **Back to app** does, instead of discarding them.
- **Failed role and override saves now say so, and a refused purge no longer dead-ends** (internal development reference omitted) — creating, editing or deleting a role, and saving a channel or category permission override, used to fail without a message. The editor now shows the error. Purging a channel or server on a server that requires MFA used to stop at a screen with nothing to enter or ended as "forbidden". It now asks for a code or security key, or tells you to set up verification and links you there.
- A delayed failed message edit no longer closes an edit in another chat panel or loses its unsaved text. The failed draft remains available when reopened while its message stays mounted.
- **The microphone resumes after a settings change with the Dynamic noise gate** (internal development reference omitted) — with the noise gate set to Dynamic, changing a microphone setting or device during a call could stop before the microphone was turned back on.
- **AGC failure holds pause** (internal development reference omitted). If AGC fails, unmute and call restoration keep the previous unprotected microphone paused. It stays paused while AGC remains enabled.
- **Signing in with an email code now sends the code** (internal development reference omitted) — when the sign-in screen, or a session check, asks for a code from your email, the app now asks the server to send it. Before, the email never arrived and the code could not be entered. If sending fails, **Send a new code** asks again. While a session check is open, the app sends its code and your answer only to the server that asked for them, even if you switch servers.
- **Sign-in offers backup codes only when they can work** (internal development reference omitted) — backup codes belong to the authenticator app, so the sign-in screen no longer offers them when the app is set to recovery only, or when no authenticator app is set up; in both cases a correct code was rejected as invalid. Confirming a sensitive change in Settings still offers them whenever the app is set up, recovery only or not.
- **Session checks can use your security key** (internal development reference omitted) — when the app asks you to verify a session, or to finish signing in with Google or Apple, choosing Security Key / Biometrics now starts the verification instead of showing a placeholder. The app uses your security key only for the server you are signing in to: a server that asks for another server's key is offered your other methods instead, so it cannot pass the result on to that server. On a self-hosted server whose security key is registered to a parent domain of its API address, the check offers your other methods instead. If the server refuses your security key, the check now says why and asks you to try again, instead of waiting. The check opens on the method you used most recently, as sign-in does, instead of always starting the security key.
- **Voice-join errors stay out of the toolbar** — microphone and other join failures now appear in a small centered prompt, without pushing the client layout down. Dismiss or Escape closes the prompt.
- **Voice joins capture the selected microphone once** (internal development reference omitted) — server and direct-message calls capture the selected input after room and encryption setup. If capture finishes after a join has ended, or encrypted publication fails, Concord stops the track. A capture error now gives you useful next steps; the device may be unavailable or in use, so choose another microphone or close the app using it. Microphone permission errors point you to system settings. Audio setting changes preserve your mute, including when you press Mute during a microphone update, and changing an input or audio setting can retry a failed microphone update during a call. Speaking feedback keeps working after switching or rebuilding the microphone, including while alone in a call.
- **Audio menus now show one Default option** (internal development reference omitted) — it follows your operating system's default device, while named microphones and speakers remain selectable.
- **Camera replacements and call cleanup preserve the right video** (internal development reference omitted) — local previews survive swaps, and remote video turns on only after its replacement consumer attaches. A remote tile can briefly disappear if its old producer closes first; room-level debounce keeps that gap from disabling layered camera publishing. Queued camera loads are canceled when their producer closes or their owner leaves before the stream attaches. Late media from an earlier voice session is closed before it can attach after a rejoin.
- **Settings sections start collapsed and stay the way you leave them** (internal development reference omitted, internal development reference omitted) — every section in Settings now opens collapsed, including the six that used to open expanded. A section you expand stays expanded when you switch panes or close and reopen Settings, until Concord Voice restarts or reloads, so you can go test something and come straight back to the section you were working in. Expand all, the sidebar links and the update indicator remember what they open the same way.

### Changed

- **New-account privacy defaults** — Friend requests start at Mutual Servers, Server Voice details start off, Private Call presence starts at Friends, and notifications show the sender without message text. Existing saved choices remain unchanged.
- **Friend discovery and username changes** — Under Mutual Servers, people who share a server or mutual friend with you can request you, and someone with a valid friend code can too. No One and blocks still stop requests. Username changes use six calendar months on Free and three on Premium.
- **Advanced audio DTX** — Turning DTX off in Advanced settings now disables it, even when the selected Basic quality preset normally enables it.
- **Noise Gate adds Dynamic and explicit modes** (internal development reference omitted) — Dynamic learns from microphone levels during an active call or Microphone Test and favors letting sound through when background noise and speech overlap. Manual Calibrate keeps a fixed dBFS threshold you set; Off leaves the gate open. With automatic gain control (AGC) on, the choices are Dynamic and Off. With AGC off, including Music Mode, Manual Calibrate is available too. Mode and threshold follow Apply/Revert. The guided Auto Calibrate test will follow separately. Recheck a manual threshold after changing your room, microphone position, hardware, processing, or Microphone Level. AGC still limits outgoing sample peaks to −6 dBFS; the gate controls activation, not microphone loudness.
- **One confirmation prompt for purges, purge protection and DM Clear** (internal development reference omitted) — purging a DM or group chat, turning off purge protection, and clearing a DM's history now share one prompt that asks only for what your account uses. An account without an authenticator app or security key no longer sees a code box it cannot fill, a passkey or security key works in one step, and the prompt opens on the method your account used most recently. With purge protection on, Clear goes straight to that prompt instead of first sending a request it knew would be refused.
- **Security settings and session prompts ask only for what you use** (internal development reference omitted) — Security settings, session management, backup-code regeneration, Recovery Circle and the delete and purge confirmations now ask only for what your account uses, and a passkey or security key works there in one step. On a server that requires two-factor authentication, an account without it is told to set it up instead of being asked to retry. Pressing Enter in the password or code field confirms, and an expired confirmation when clearing a direct message's history now says so on the password field. If your session ends while one of these prompts is open, it says so and stops asking, and a code you typed is kept when you are asked to wait before trying again.
- **AGC now limits microphone sample peaks** (internal development reference omitted) — calls and Microphone Test use the same microphone processing path. When automatic gain control is active, Concord Voice limits samples leaving its microphone processor to −6 dBFS, with about 4 ms of lookahead and 100 ms recovery. This does not repair clipping already present in the source or measure acoustic loudness. The Off and fixed-threshold noise-gate choices remain available; the fixed gate now measures float samples in the processor.
- GIF personalization now uses the Personalization ID shown in Settings to derive the separate ID sent to KLIPY. When personalization is off, GIF browsing requests use a temporary ID that changes about every 30 minutes; sharing a GIF does not send that ID. Turning personalization back on resumes the saved Personalization ID unless you rotate it. The Recent tab is available only while personalization is on. After you click Rotate while personalization is on, requests using your saved ID wait for the new ID, which starts a new Recent list. An open GIF picker refreshes when the setting or saved ID changes.
- **Audio and video device controls now sit with their settings** (internal development reference omitted) — Input and Output selectors, microphone and speaker tests, and output level start Audio Configuration; Camera selection, test, and preview start Video Configuration, removing the separate Device Configuration section. Device choices, including Default, apply immediately; processing and Microphone Level use Apply/Revert. The app-only Microphone Level defaults to 100%, spans 0–200% by 1%, and appears when effective automatic gain control (AGC) is off, including Music Mode; its saved value persists while hidden. While AGC manages the microphone, positive saved levels are ignored and app gain stays at unity; saved 0% remains silent. The saved level applies again when AGC is off or Music Mode is on. Microphone Test uses applied settings. Section close and call conflicts stop tests; mic-input changes replace active test capture, while output/camera changes stop theirs. Level is applied before the saved gate; a non-unity level changes gate activation without recalibration, so apply and retest it.
- Webcam speaker frames stay at a fixed size and show a steady accent highlight during voice calls. Avatar frames keep their speaking pulse (internal development reference omitted, internal development reference omitted).
- Refreshed the desktop icon library to lucide-react 1.48.0 (internal development reference omitted).
- **Electron updated to 44.5.0** (internal development reference omitted).
- **Voice-call connection libraries updated** (internal development reference omitted) — mediasoup-client 3.24.1 and socket.io-client 4.8.4.
- **HDR codec controls are separate** — Enable HDR allows supported HDR-capable codecs. Prioritize HDR Codecs prefers an HDR codec over hardware acceleration when both are available. If a codec check finishes after your camera or screen share starts, Concord Voice updates the active stream's codec choice. Neither setting guarantees HDR capture or display.
- Voice, camera, and screen sends now use separate plan-based bitrate ceilings. The desktop lowers its send settings when a plan changes. When measured video use repeatedly exceeds an admitted limit, the server turns off video for a temporary cooldown while voice remains available. Closing a video stream during a server measurement does not erase its measured usage.

### Security

- **Control plane built with Go 1.26.9 and `golang.org/x/net` 0.60.0** (internal development reference omitted) — closes the standard-library and HTTP/2 advisories published on 2026-10-08, including HTTP/2 server memory exhaustion from trailer headers, an HPACK encoder race that can crash the server, unbounded Range header parsing, and HTTP/1 connection desynchronization after a rejected CONNECT.
- Disabled self-host feedback now refuses submissions before processing their contents or sending them to GitHub. The fresh installer rejects invalid feedback settings, SMTP host/sender inputs and proxy range combinations that trust every source before saving configuration, and refuses ambiguous retained data while preserving it for reviewed recovery. Self-host lifecycle commands use saved configuration rather than inherited shell overrides. Certificate import checks every required service name before installing the pair. Public self-host release publication requires nonempty cited proof files, reconstruction with current trusted tooling, and fresh asset bytes and attestations bound to the accepted release commit before promotion; it verifies the immutable result before reporting success.
- Updated Vite to 8.3.3 for desktop and admin development builds, closing three development-server advisories: [file disclosure](https://github.com/vitejs/vite/security/advisories/GHSA-rq7h-c2jc-7f22), [Wasm-query file disclosure](https://github.com/vitejs/vite/security/advisories/GHSA-vfpm-58rq-9qcg), and [cross-origin script execution](https://github.com/vitejs/vite/security/advisories/GHSA-9jrq-w75r-8gcw).
- **Patched the desktop `source-map-js` dependency** (internal development reference omitted) — updated from 1.2.1 to 1.2.2 to address malformed source-map denial of service ([CVE-2026-93749](https://github.com/advisories/GHSA-68fv-2mgg-jv7q), internal development reference omitted).
- Updated the desktop development dependency `shell-quote` to 1.12.0 to address shell command injection in comment-bearing argument lists ([CVE-2026-102422](https://github.com/advisories/GHSA-pqg4-j6r4-53mv), internal development reference omitted).
- **Media server dependency security update** (internal development reference omitted) — updated the library that interprets forwarded client addresses to its patched release, and patched `source-map-js` and Vite in media development tooling. No configuration change is needed.
- **Trusted-device recovery requires matching fingerprints on both devices** (internal development reference omitted) — compare all eight groups and confirm the match on each device before your account key can be transferred and opened. Recovery keeps your original account key and message access. Updated servers refuse the old recovery protocol, so update and restart both devices and begin a fresh request. The server update clears pending recovery requests; it does not remove accounts, trusted devices, recovery circles or account keys.
- **Security-key confirmations stay with the server that asked** (internal development reference omitted) — when you confirm an action such as a purge or clearing a DM's history with your passkey or security key, the app now uses your key only for the server you are on. A server that asks for another server's key is refused before your key is touched, so it cannot pass your confirmation on to that server, and options a server adds to the request no longer reach your key. On a self-hosted server whose key settings name its parent domain rather than its API address, a security-key confirmation now reports that the key could not be verified.

## [0.2.48] — 2026-10-04

Concord Voice now keeps screen sharing steady when a capture changes, cleans up
cancelled audio work before it can surface later, records security outcomes
without losing the observation that triggered them, pauses a voice stream
that keeps sending far more than your plan allows, and asks for the right
proof of identity in the right places — never a code an email/text-only
account can't provide, never your password twice.

### Added

- **Bug reports can now include screenshots** (internal development reference omitted) — the in-app bug-report form now lets you attach up to four images (PNG, JPEG, or WebP, up to 5 MB each), so a visual glitch can be shown rather than described. Attached images are re-encoded before upload, which removes embedded camera metadata such as GPS location, and they appear inline in the filed report.
- **Deleting many messages quickly now asks you to confirm it is you** (internal development reference omitted, internal development reference omitted) — if you delete more than 15
  messages within 30 seconds, or more than 100 within 24 hours, the app asks for your authenticator or
  security-key code, or your password if your account has no code method, before it deletes the next
  one. Purging your own messages from a channel or server counts the same way; a purge of more than 15
  of your own messages always asks. The rule applies when your "Require authentication before purging"
  privacy setting is on (the default) and you delete your own messages, and on servers that require MFA
  for dangerous actions. If you cannot confirm, the app shows how long to wait. Deletes that were
  refused used to fail silently on the desktop; each one now opens a dialog. Your password is sent only
  to a new confirmation endpoint, never to the delete, purge or clear request itself — DM "Clear
  history" included — and a security-key confirmation is no longer used up by an attempt that fails
  partway. **API clients:** a delete or self-purge request may carry an optional JSON body
  `{"mfa_code", "step_up_token"}` (up to 4 KiB). An account without inline MFA gets the token from the
  new authenticated `POST /api/v1/auth/step-up/password` with `{current_password, purpose}`: it shares
  login's lockout (423 `account_locked`) and rate limit, refuses an account with MFA (403
  `mfa_required` with `mfa_methods`), and returns a single-use `{step_up_token, expires_in: 60}` bound
  to one route's purpose. It allows 10 attempts per 15 minutes per IP address and 10 per account,
  answers 503 rather than skipping those limits when it cannot check them, refuses an account that
  has neither a password nor MFA with the target route's own 400 message (flagged
  `step_up_unavailable: true`, as is that route's own 400 for the same account), and records every wrong
  password, lockout and success as a security event the way login does. In the app, a password
  prompt whose account has since gained an authenticator moves to the code prompt instead of a dead
  end, a server too old to offer the endpoint says so instead of blaming your password, the password
  field is emptied after each attempt, a purge that finishes after you closed its dialog no
  longer reports its result in the next one, and if another account signs in, or another server is
  selected, while your password is being confirmed, the delete, purge or Clear is not sent at all. DM Clear takes `step_up_token` in place of `current_password` too, and any
  delete, self-purge or Clear body that still carries `current_password` is now a 400. A WebAuthn
  confirmation is spent inside the request's own transaction, so a request that rolls back leaves it
  usable (migration 000162). A channel or server purge body must now be exactly one JSON object: a second JSON value
  or trailing bytes after it are refused with 400. New refusal bodies on the channel and DM message-delete routes and the channel and server
  purge routes: 403 with `delete_rate_limited: true` and a `Retry-After` header (`mfa_required` with
  `methods`, `Invalid MFA code`, `password_required` — with `step_up_token_invalid` when the token sent
  matched nothing — or `mfa_enrollment_required`);
  429 with `step_up_budget_exhausted: true` when too many factors were sent; and three different 503
  bodies: soft-lock unavailable (`Retry-After: 30`, no flag), `step_up_budget_unavailable: true`, and
  `lock_conflict: true` (`Retry-After: 1`). While Redis refuses writes, deletes by users the rule covers
  return the first 503. The shared step-up budget's 429 and 503 now carry those two flags on every
  route that charges it: the MFA settings routes, the "Require authentication before purging" setting,
  and the server MFA-enforcement toggle.

### Changed

- **Voice now pauses a stream that keeps sending far more than your plan allows** (internal development reference omitted, internal development reference omitted) — the voice server now
  measures how much each person actually sends. A microphone, camera or screen share that keeps sending
  well above what your plan allows is paused, and you see a notice saying which one and what to do;
  everyone else just sees it as muted or paused. If it happens a second time within an hour, you are
  removed from the call and can rejoin after 15 minutes. The Concord app stays within these limits on
  its own: screen sharing with sound now sends at your plan's audio quality (96 kbps on the free plan)
  instead of an uncapped rate.
- **The control plane now supports private DM hide and history-clear operations** (internal development reference omitted) — authenticated API clients can hide a direct message or group chat from one participant's list, or clear that participant's history before a server-stamped cutoff. Hide preserves history, read state, and the original hide timestamp on retries. Clear uses MFA when enabled or the current password otherwise when Privacy & Security requires authentication; the existing permanent purge action keeps its current password-and-MFA behavior.
- **Server owners can require MFA for dangerous actions, through the API** (internal development reference omitted, internal development reference omitted) — a server's owner or an Administrator can turn on `PUT /api/v1/servers/{id}/mfa-enforcement`. While it is on, a member without an authenticator app or security key cannot use the dangerous permissions: managing the server, its roles or its channels, kicking, banning, deleting other members' messages, rotating channel keys, and managing developer resources. Email and text-message codes do not count. That member keeps every other permission and still sees every channel they could see before. Turning it on requires your own authenticator app or security key; turning it off requires a code from one. The app has no setting for it yet (internal development reference omitted), so for now only API clients can change it.
- **Direct messages now offer separate Hide, Clear history for me, and Leave group controls** (internal development reference omitted) — Hide removes a thread from your list until a new message arrives; Clear removes its earlier messages only from your view and asks for account verification when required. Leaving a group ends your membership and access to its messages. Purge Messages remains a separate action.
- **Purge Messages now uses one range menu and says up front what it deletes** (internal development reference omitted) — the time range is a single dropdown instead of nine buttons, and in a direct message or group the dialog now says before you choose a range that only your own messages are removed; everyone else's stay for them and are hidden only from you.
- **Hide now looks like the reversible action it is, and Leave group has its own section** (internal development reference omitted) — the Hide confirmation no longer uses the red warning styling of Clear history and Leave group, and Group Info now places Leave group in its own section, styled as destructive and separated from Purge Messages.
- **Font Size is back under Appearance** (internal development reference omitted, internal development reference omitted) — the
  Small / Default / Large text size now sits in Appearance ▸ Application Font, next to the font it
  changes, instead of in Accessibility ▸ Display. The UI Scale slider stays in Accessibility. Font
  Size stays usable while Dyslexic Support is on, and screen readers now announce which size is
  selected.
- **UI Scale now runs from 50% to 200% and scales everything** (internal development reference omitted, internal development reference omitted) — Accessibility ▸
  Display ▸ UI Scale now zooms the whole interface, text, icons and layout together, instead of
  enlarging text inside panels that stay the same size. Above 100% the zoom is limited by window
  width so the layout never gets too cramped to use, and the slider tells you when that happens and
  how far it would go in a wider window. Picture-in-picture windows keep their normal size. A
  desktop app that has not updated yet keeps the previous 85–130% range.
- **Your font choice now reaches headings, navigation and messages** (internal development reference omitted, internal development reference omitted) —
  Appearance ▸ Application Font has two modes. **One Font** applies your pick across the app,
  including headings, the server and channel lists and the member list, which used to keep the
  brand face. **Font by Area** sets separate fonts for Messages, Headings, Navigation and
  Interface. The Concord Voice wordmark keeps its brand face unless Dyslexic Support is on. Menus,
  profile cards and other people's profiles now follow your font too, and so do buttons and text
  boxes that used to show the system font, even with Dyslexic Support on (for example "Forgot
  password?" on the sign-in screen). Those controls are the one change you will see if you never
  picked a font.
- **Application Font now shows your mode and says what each default does** (internal development reference omitted) —
  the One Font / Font by Area switch highlights the mode you are in again. Headings, Messages and
  Navigation now default to **Match Interface**, and a Headings default follows your Interface font
  in both modes. The Interface list starts with two defaults: **Theme Default** uses your theme's
  font (Atkinson Hyperlegible Next with Agency, Concord's own fonts with every other theme), and
  **Concord Voice Default** keeps Concord's own fonts whichever theme you choose; each default
  now has a one-line description. While Theme Default is selected, a small "Active with the
  current theme" tag marks the font in use. The selected side of the One Font / Font by Area,
  Audio and Video Basic / Advanced switches now also has an outline, so it stands out in every
  theme.

- **Setting up an authenticator app now tells you what happened to your recovery key** (internal development reference omitted) — after
  you finish adding an authenticator app, Concord shows whether it created a new recovery key, kept
  the one you already had, or could not create one, instead of moving on without saying. If
  creating one fails you can try again or continue without it. Replacing a key you already have
  asks for your password and an authenticator code.

- **DM-block and credential-epoch cleanup now survives delivery failures** (internal development reference omitted) — guarded reconciliation records retryable voice-ejection obligations and fences stale callbacks by generation.
- **Direct-message writes recheck current membership before durable side effects** (internal development reference omitted) — removed recipients cannot regain keys, enroll pending keys, or attach encrypted uploads through stale authorization.
- **Server mutations now serialize authority and membership changes at their write boundary** (internal development reference omitted) — stale role, ownership, channel, and message operations fail closed instead of committing partial state.
- **Voice joins now verify admission before opening a session** (internal development reference omitted) — voice admission, disconnect cleanup and moderation updates now agree on the same current session, so an old disconnect cannot remove a newer join and everyone sees the same moderation state.

### Fixed

- Screenshots removed while diagnostic logs are being collected are no longer included in the submitted bug report.
- **Messages and attachments always carry the version of the key that encrypted them** (internal development reference omitted) — if the app's copy of a channel or conversation key changed between encrypting a message and sending it, the message could go out labelled with a different key version than the one it was encrypted under. Depending on the timing, the connection dropped and the message had to be sent again, or the people you sent it to could see "Unable to decrypt". Direct messages, channel messages, messages queued while offline and attachments now take the key and its version together, in one step. When you send several attachments at once, each one now reads the current key as it starts uploading instead of reusing the key from the start of the batch, so if someone is removed partway through, files that start after the new key is in place can't be opened by them. A file still uploading at the moment the app learns of the removal now stops with an error instead of finishing under the old key; send it again.
- **A purge target now names the same author at admission and deletion time** (internal development reference omitted) — channel and server purge requests now normalize a valid `target_user_id` before authorization; a malformed target returns 400.
- **Self-hosted servers keep Google and Apple sign-in unavailable** (internal development reference omitted, internal development reference omitted) — the server omits both sign-in options and refuses their sign-in endpoints, even when provider settings are enabled. Existing accounts can still sign in with their password and complete two-step verification; settings cannot require an unavailable SSO provider. Unlinking the final old identity also preserves password sign-in if the server later returns to SaaS.
- **Remember Me no longer signs you out when your keychain is briefly unavailable** (internal development reference omitted) — if the system keychain could not unlock your saved sign-in at startup, the app deleted it and you had to sign in again. It now keeps it and tries again next launch.
- **Email MFA can be turned on again, and SMS shows as coming soon** (internal development reference omitted) — switching on email codes in Settings ▸ Multi-Factor Auth failed for every account, because a hardened-recovery setting that was on by default also asked for a text-message code, and text-message codes are not available yet. Email codes now turn on with the email code alone. The SMS option and the hardened-recovery switch, which needs it, are marked "Coming soon" until text-message codes launch.
- **A hidden conversation now comes back as soon as someone messages it** (internal development reference omitted) — after you hid a direct message or group, a new message (or call) brought it back only after you restarted the app; it now reappears right away with its full history.
- **Group messages from before someone left are readable again** (internal development reference omitted) — when a
  member left or was removed from a group chat, the people who stayed saw
  "Unable to decrypt" on every earlier message after reloading, and in the
  conversation list, even though they still held the key for them. Earlier
  messages, replies, pins, search results and the conversation-list preview now
  open with the key they were written under.
- **Requests to a server that is deleted mid-request no longer fail with a server error** (internal development reference omitted) — if a server is deleted, or its owner's account is erased, while a request to it is in flight, permission and membership checks and role, channel-permission-override, channel-sync, channel, channel-group, member-add and member-moderation requests now answer as they would for a non-member instead of returning 500. Most now return 403, including creating or reordering roles, adding or banning a member, role unassignment, channel-override deletion and synced channel moves; the last three, and a role reorder, returned 404 in that window ("Role not found" for the reorder), as did a ban by an owner whose own account was being erased; changing a member's role, timing out or removing a member returns 404 "User is not a member of this server", renaming or reordering a channel group returns 404 "Channel group not found", and a server's visible-channel list comes back empty. Relatedly, a database failure while checking membership when changing a member's role or removing a member, or while checking the role hierarchy when removing, timing out or banning a member, now returns 500 instead of wrongly answering 403 or 404, even when the server is deleted at the same moment; a role reorder whose actor's account disappears while the server remains returns 500 rather than 404 "Role not found". And if the database fails to discard an interrupted change to a server's members, roles or channel groups, or an interrupted ban or block, the request now returns 500 instead of the refusal the change had reached.
- **Buttons and avatars on the brand colours are readable in every colour scheme** (internal development reference omitted) — the label on Continue, Sign In, Send and other brand-coloured buttons, and the initials on brand-coloured avatars, could nearly disappear: in Concord light they measured 1.20:1 against the fill, and 18 of the 32 scheme and theme combinations fell below the 4.5:1 readability line. Each scheme now pairs its brand fill with a text colour chosen against the whole gradient, so every combination clears 4.5:1. Pride keeps its flag under a dark tint with white text, and Spooky and Cotton Candy light use slightly darker gradients. Custom themes get the same guarantee. Hovering a brand button, the Create Channel and system-permission buttons, or the reset-keys button now lifts it instead of fading it. Initials on another member's avatar colours now use a label chosen for those colours rather than your theme's (white on a green avatar read 1.37:1). The Cancel button in the permission-override editor, the account-reset button and the skip-recovery-key button keep a readable label too, and so do the initials and name on a voice tile that dims while someone else speaks. Offline members and friends, and outgoing friend requests, no longer fade their initials and name: only the avatar picture dims. Opening Server Settings no longer restyles the invite window's Generate button, and the Server Settings invite buttons keep a readable label on hover (they read 2.19:1 in Concord light).
- **Error text in code and security-key prompts is easier to read** (internal development reference omitted, internal development reference omitted) — the red error message under
  a code or security-key prompt failed the 4.5:1 contrast minimum in 9 of the 30 theme and light or
  dark combinations (as low as 3.45:1 in Spooky dark). It now uses the primary text colour, so it
  reads in every theme; the message text still says what went wrong.
- **A failed voice join no longer removes someone who joined the next call** (internal development reference omitted, internal development reference omitted) — delayed cleanup now applies only to the call that failed.
- **Text-only channel changes work on busy voice servers** (internal development reference omitted) — they no longer scan every active voice channel, and deleting a channel group retries if a child becomes a voice channel during the change.
- **Voice access cleanup no longer sends an in-flight voice update to someone whose access was revoked** (internal development reference omitted) — temporary access removal holds the audience check through the database change.
- **A malformed server response no longer replaces the desktop's working configuration** (internal development reference omitted, internal development reference omitted) — the app keeps the last valid settings for that server, or safe defaults after switching servers, until a valid response arrives.
- **Channel and category permission overrides save again, and a failed save now says so instead of closing** (internal development reference omitted, internal development reference omitted) — every
  override save from the app was being refused, and the editor closed as if it had worked. Saving,
  adding and deleting an override now works. If the server refuses one, the editor stays open with
  your changes and a message saying it did not save. While a server is still on an older release,
  an override the app cannot read exactly is marked "Can't be shown" and can be deleted but not
  edited, so saving it can never drop permissions it held.
  **Breaking change for API clients:** every permission bitfield on the RBAC endpoints now crosses
  JSON as a decimal string. That covers override `allow`/`deny` in both directions, `permissions`
  from `GET /servers/{id}/permissions` and `GET /channels/{id}/permissions`, and the bitfield keys in
  audit-log `metadata`. The role `permissions` field already worked this way. A JSON number in an
  override request is now refused with 400, because numbers above 2^53 lose their low bits in
  transit. Send `"allow":"1024"`, not `"allow":1024`.
- **Signing in with two-factor authentication no longer hangs when your encryption keys can't be unlocked** (internal development reference omitted, internal development reference omitted) —
  the offer to reset your encryption keys appeared only on the password page, so an account with
  two-factor authentication waited on it forever after entering its code. It now appears on the
  two-factor page too, with its code field and buttons styled to match the app. A code the server
  refuses at sign-in is also cleared from the boxes, since it cannot work a second time.
- **Regenerating backup codes from Settings works again** (internal development reference omitted, internal development reference omitted) — the app sent the code
  from your authenticator app in a field the server does not read, so every regeneration was
  refused. It now sends the code where the server expects it.
- **Server owners can mention @all and @here again** (internal development reference omitted, internal development reference omitted) — the
  permission set the server gives a server's owner did not include the right to mention everyone, so
  an owner's @all or @here was silently removed from the message unless one of their roles also
  granted it. Owners now always have it.
- **Try again on the recovery-key screen now shows each attempt** (internal development reference omitted) — when creating a recovery
  key failed again after Try again, the screen looked exactly the same, so the button seemed to do
  nothing. The message now counts the attempts.

- **Reset TOTP now accepts a typed backup code** (internal development reference omitted) — a backup code typed into Reset TOTP was only
  picked up if you pressed Enter, so clicking Confirm sent no code and failed with "Password and MFA
  code are required". The code is now used as soon as all eight characters are typed, and Confirm
  waits until a code is entered. When signing in, a backup code is now submitted as soon as its
  eighth character is typed, the same way an authenticator code is.

- **Rich Presence sharing settings save every time** (internal development reference omitted) — changing who can see your Server Voice or
  Private Call activity worked once and then failed on every later try, snapping back to the old
  choice. Each failed try also briefly disconnected everyone whose app was connected to the same
  Concord backend.
  Changes now save every time. While you are in voice or a call, only the people who could see
  that activity briefly reconnect; otherwise nobody else is disconnected. If a setting of yours is
  stuck, the next change fixes it.

- **Purging a direct message can no longer hang** (internal development reference omitted) — Purge Messages in a
  direct message or group chat now stops after 10 seconds, the same limit channel and server purges
  already have. If it runs out of time you see that some messages may already have been purged, and
  the conversation refreshes. Messages it had already removed also disappear for the other people in
  the conversation straight away, instead of only after they reload.
- **The "Verify Your Identity" prompt can no longer get stuck behind Settings** (internal development reference omitted) — when Concord
  asked you to confirm your identity while Settings or another dialog was open, the prompt opened
  underneath it and could not be reached, and the action you were taking stayed on "Processing…"
  until you reloaded. The prompt now opens on top and takes the keyboard, and Escape cancels it. It
  stays on top if another dialog opens while it is showing, and a new prompt no longer starts out
  locked, or showing an error, left over from an earlier one. It is also styled correctly when it
  appears at sign-in, before Settings has ever been opened, and it now appears if Concord asks
  while it is still starting up. Escape, Tab and the arrow keys reach it even with a sidebar,
  menu, popup, picker, image viewer or another dialog open behind it, the mute and deafen
  shortcuts still work while it is showing, it fits a zoomed or small window, and a check that
  takes too long stops with a message instead of locking the prompt. A wrong code now says what
  went wrong, including for single sign-on accounts, and puts the cursor back in the code box.
- **Turning on two-factor authentication no longer asks for your code a second time** (internal development reference omitted) — after
  you set up an authenticator app, a security key or email codes, Concord used to ask for a code
  again a few minutes later on the same device. The device you set it up on is now trusted straight
  away. Your other signed-in devices still confirm the new factor once, as before. If setup hits a
  server error, trying again now finishes it instead of saying it is already on, and a brief server
  fault during setup can no longer switch two-factor authentication off without telling you.
- **Voice activity now clears before a privacy change reconnects the app** (internal development reference omitted) — lowering Rich Presence visibility,
  leaving a voice session, or disconnecting can no longer briefly deliver an older activity
  update after its removal.

- **Update, reconnection and what's-new windows now work while Settings is open** (internal development reference omitted) — when Concord required an
  update, lost its connection, rejected an unofficial build or showed what's new while Settings or
  another window was open, the buttons in that window could not be clicked or reached with the
  keyboard, and Escape or Tab acted on the window behind it. These windows now always open on top
  and take the keyboard, and keyboard shortcuts such as the one for Settings no longer open
  anything over them. Pressing Escape in the update or reconnection window no longer closes a
  window hidden behind it, and screen readers now read each window's message and announce when
  the connection state changes. The encryption key recovery prompt now opens centred instead of
  in the top-left corner, and the "Download Official Client" button label is readable again in
  dark themes.

- **The Basic / Advanced Settings switch in Audio and Video Configuration now works with a keyboard and
  screen reader** (internal development reference omitted) — screen readers announce the two options as a choice of two instead of as tabs,
  the arrow keys move between them, Tab stops once per switch, and a focus ring shows which option
  has keyboard focus. It looks the same as before.

- **A damaged two-factor record no longer crashes sign-in checks or the MFA key-rotation tool** (internal development reference omitted) — if the
  stored authenticator-app secret for an account was damaged, checking that account's two-factor
  code crashed the request, which answered with an empty server error. It now fails with a normal
  error message, and the code is still refused. The `mfa-rekey` tool, which moves stored secrets to
  a new encryption key, used to stop at such an account and leave every account after it on the old
  key. It now reports that account as failed and carries on with the rest. Accounts whose
  two-factor record is intact are not affected.

- **Screen sharing no longer starts free accounts on a Premium resolution** (internal development reference omitted) — on a
  display larger than 1080p, the screen-share picker opened on "Source Native (Premium)" for free
  accounts and then quietly shared at 1080p. The picker and Settings ▸ Audio & Video now select
  1080p instead, and show Source Native as a Premium option you cannot select. Upgrading brings
  Source Native back without changing any setting.

- **Premium labels and Admin badges are readable in every color scheme** (internal development reference omitted) — the Premium lock
  label, the Premium badge in Settings and the Admin badge on member profiles drew white text on
  yellow in light themes, and black text on the dark accent some color schemes use. Their text now
  switches between black and white to stay readable in every color scheme, including High Contrast
  and custom themes.

- **Links opened from a picture-in-picture window now open in your browser** (internal development reference omitted) — a link or pop-up
  started inside a picture-in-picture window could open a new Concord window. Picture-in-picture
  windows now follow the same rule as the main window: secure (`https`) links open in your browser,
  and anything else is blocked.
- **Sharing an app with its sound now tells you when the sound could not be captured** (internal development reference omitted) — when Concord could not
  capture an app's sound (for example a Finder window, which has never played audio), the share
  could still show app sound as on while none was being sent. It now says "We couldn't capture that
  app's sound." Reloading Concord, or its window crashing, during a share now also stops capturing
  that app's sound straight away; before, the capture kept running in the background until you
  quit. Starting a share with app sound can take a moment longer, because it now waits until the
  sound capture is ready.
- **Sharing an app window with its sound now captures apps that play sound from a helper process, and tells you if the sound stops** (internal development reference omitted, internal development reference omitted) — on
  macOS, many apps play sound from a separate helper process rather than from their window, so
  sharing one of those windows sent silence. Concord now captures sound from the app's helper
  processes too, but never from Concord itself, so your call is not echoed back to the people in
  it. If app sound stops partway through a share, the Share sound button shows a marker and a
  message tells you, while your screen keeps being shared; hover the marker to read the message
  again, and turn Share sound back on to try again.
  Safari's sound still cannot be captured, and Concord now says so ("We couldn't capture that
  app's sound") instead of sharing silence without a word. Voice bar error messages now sit on a
  solid background, so the video behind them no longer shows through the text.
- **Empty, hidden direct and group conversations are cleaned up** (internal development reference omitted, internal development reference omitted) — after everyone has hidden a conversation and no messages remain, Concord removes its unused server data. Pending or active voice calls keep the conversation intact. The last member of a group can also leave when nobody remains to take ownership.

- **Keyboard focus rings are visible in every theme and no longer look like a selection** (internal development reference omitted, internal development reference omitted) — the ring or border that shows which control has keyboard focus now uses the theme's focus colour everywhere, instead of the accent colour that also marks the selected tab or row. It is readable against every background in all 32 theme variants, including the default light theme, where some rings were a pale yellow that was nearly invisible. The voice-volume slider now shows a solid ring when focused instead of a faint glow.
- **Moving between voice channels on different servers no longer loses the old-channel leave** (internal development reference omitted) — the old channel's participant update now waits for the move to commit, so its count cannot be left stale by a fast delivery.

- **The Concord logo on the sign-in screens is readable in light themes** — the logo's
  lettering was drawn in white, so with a light theme selected the word CONCORD and the
  "Own your voice" line underneath all but disappeared, leaving only the moon. On a pure
  white background it vanished completely. The sign-in, sign-up, account-recovery and
  first-launch screens now show a dark-lettered version of the logo whenever a light theme
  is active, and the original white one in dark themes. It also follows your system
  setting if you have the theme set to match it, switching as soon as the system does.
- **Right-click menus no longer open partly off-screen** (internal development reference omitted, internal development reference omitted) — a tall menu opened near
  the bottom of the window could spill past the top edge and hide its first items. Menus and their
  submenus now stay on screen, and one taller than the window scrolls.

- **Voice channel lists recover more reliably after stale sessions** (internal development reference omitted, internal development reference omitted, internal development reference omitted) — the server retries delayed leave updates, and repeated updates no longer lower the displayed participant count or replay leave sounds. A delayed old-channel leave can no longer overwrite a participant who has already moved or been removed.

- **Dirty database migrations now require an explicit operator recovery** (internal development reference omitted) — control-plane startup stops rather than guessing whether a partially applied migration can be replayed.

- **Switching screen-share sources no longer drops or leaks per-app audio** (internal development reference omitted) — a failed
  source switch now keeps the current share running, while cancelled or partially started captures
  release their audio process instead of publishing it later or leaving it behind.
- **Returning to the latest message now stays there while older chat history loads** — if
  a saved reading position was on a page that had not arrived yet, choosing Return to Latest
  could take you back to that old message as it loaded. The chat now keeps you at the latest
  message instead.
- **A saved chat position now waits for the right history page** — returning to a conversation no
  longer gives up when the first page does not contain the saved message. Concord keeps the saved
  position pending and restores it when a later page you load includes that message. It waits for
  the initial history request to settle, including after a recoverable failure, and it does not
  fetch extra pages by itself.
- **Dyslexic Support now changes every piece of text, not just some of it** (internal development reference omitted) — turning on
  Dyslexic Support said it applied "across the app", but headings, the server and channel list
  down the side, and the window title all kept the old lettering. Only ordinary body text
  changed. Now every one of those follows it too, including on themes that bring their own
  heading font.
- **Choosing an app font now changes text boxes, buttons and menus as well** (internal development reference omitted) — picking a
  font under Appearance left a great deal of the app behind: anything you type into, every
  button, the right-click menus, and most of Settings stayed in the original font. It was easy
  to miss, because the font they were stuck on was the default one — so nothing looked wrong
  until you picked something else. They all follow your choice now.

- **Inviting someone to a server they are already in is greyed out** (internal development reference omitted) — the
  invite list offered every server you could invite to, including the ones the person you were
  messaging had already joined, so the only way to find out was to send an invite they could not
  use. Those now appear greyed and marked "already a member". In a group message a server is
  greyed only when _everyone_ in the conversation is already there, since a server one person has
  joined is still worth inviting the rest to. If the list cannot be checked — an older self-hosted
  server, or a brief network problem — nothing is greyed and the invite works exactly as before.
- **The invite button in a direct message lists every server you can invite to** (internal development reference omitted) — opening
  a direct message and pressing the invite button showed only the server you had open before you
  navigated away, and often nothing at all, so inviting a friend to any other server was simply
  not possible from there. The list is now built from the servers you are actually allowed to
  invite people to, wherever you are in the app, instead of depending on which server you
  happened to visit last. If creating the invite does fail anyway — the list is a snapshot, so a
  permission removed while you had the chat open is not reflected until it refreshes — the reason
  now appears above the message box instead of the button quietly doing nothing.
- **An invite to a server you are already in now says "Joined"** (internal development reference omitted) — an invite
  card in a chat always offered a Join button, so the only way to find out you were already a
  member was to press it and read the refusal. The card now recognises the server and shows
  "Joined", with no button to press. The same applies to an invite you open from a link outside
  the app: the join window names the server and tells you that you are already in it, instead of
  letting you try. And when a join genuinely does fail, the reason now comes from the server —
  so "you are already a member of this server" no longer arrives dressed up as "this invite may
  have expired", which used to send people hunting for a fresh invite to a server they were
  already in. One more thing the join window could get wrong: if you typed a code, changed your
  mind and typed a different one, a slow lookup of the first code could land afterwards and put
  the first server's name and icon on screen while the box held the second code — which also
  meant the "already a member" check was answering about the wrong server. It now ignores an
  answer to a code you have already replaced, so the card always describes the code in front of
  you.
- **The audio quality labels above the Audio Configuration slider now work as buttons for keyboard and
  screen-reader users** (internal development reference omitted) —
  Minimum through Studio are announced as buttons, with the current tier marked as pressed, instead
  of as tabs. Enter and Space select a tier, and the premium tiers still explain that they need a
  subscription. The slider below them is now announced as "Audio quality" with the tier's name,
  instead of a bare number. They look the same as before.

- **Accounts with only email or text-message codes are no longer asked for a code they have no
  way to check** (internal development reference omitted) — setting up an authenticator app, adding a security key, changing recovery
  settings, and the other account-security actions that ask for your password now also skip the
  code prompt for an account whose only two-factor methods are email and text-message codes,
  matching how those accounts already sign in. Transferring ownership of a server works the same
  way.
- **Removing your last authenticator app or security key while email or text-message codes are on
  is refused, with a reason** (internal development reference omitted) — removing your last authenticator app or security key while email or
  text-message codes are still on used to leave your account able to sign in with a code its own
  settings pages could no longer check. That is now refused, and the screen tells you to turn off
  email/text-message codes first.
- **Purging messages no longer asks for your password twice** (internal development reference omitted) — confirming a purge with your
  password and a two-factor code, correctly entered, no longer re-asks for the password on the
  next screen.
- **A wrong password or code now points at the field that was wrong, and screen readers read it
  out** (internal development reference omitted) — resetting your authenticator app or removing a security key with a wrong
  password or code now marks that field, clears it and puts the cursor back in it, like the other
  account-security actions, instead of showing a general error. A wrong two-factor or backup code,
  including at sign-in, is now announced by screen readers instead of appearing silently. In the
  purge dialogs, sending only your password or only a code now says which one is missing, instead
  of doing nothing.
- **Retrying a lost recovery-key save now resends the same key** (internal development reference omitted) — if the confirmation that a new
  recovery key was saved never arrived, Try Again now resends that same key instead of generating
  a different one that might not match what was actually saved.
- **A temporarily unavailable verification check now says so, instead of "too many attempts"**
  (internal development reference omitted) — a brief outage in the service that checks your password or two-factor code during a
  security action used to look identical to having tried too many times. It now says verification
  is temporarily unavailable and to try again in a few minutes.
- **Account-security actions now report a failure instead of claiming success**
  (internal development reference omitted) — signing in with a
  security key now asks you to try again when the server cannot record the key's use, instead of
  signing you in without the check that detects a copied key. Removing a trusted device, deleting a
  recovery circle and answering a recovery request now show an error when the server cannot confirm
  the change, instead of saying it was not found or already answered. An email sign-in code is sent
  at most once per sign-in attempt, even when two requests arrive together. Turning recovery-only
  mode off works again.

### Security

- **A backup code no longer signs you in past an authenticator app set to recovery only** (internal development reference omitted) — backup codes belong to the authenticator app, so marking the app "recovery only" now covers its backup codes too; before, a backup code still signed you in. A refused code is not used up, and neither is the app's code for that moment.
- **The control plane now treats an unset `ENVIRONMENT` as production** (internal development reference omitted) — it used to fall back to development, which skipped the production safety checks and allowed development-only behaviour such as logging verification codes. The bundled deploy files and the self-host installer already set `ENVIRONMENT=production`, so they are unaffected. A custom deployment that never set it will now refuse to start until it does; local development sets `ENVIRONMENT=development`. `CONCORD_ENV=test`, which writes plaintext verification codes and relaxes the sign-in rate limits, is now refused under any `ENVIRONMENT` other than `development` or `test`, not only under `production`.
- **Updated the voice server's connection library to close a published advisory** (internal development reference omitted) —
  resolves GHSA-2GC4-CQFQ-P2GV, a denial-of-service flaw in the connection that carries voice and
  video signalling.
- **An authenticator-app code now works only once** (internal development reference omitted, internal development reference omitted) — each code from your
  authenticator app is accepted once. Using it again, for a second change or on another screen, is
  refused just like a wrong code, so a code someone sees over your shoulder or in a screen share
  cannot be used after you. If you are asked for a code straight after signing in, wait for the next
  one. The app now clears a code once you submit it instead of offering it again. And if you restart
  authenticator-app setup while an earlier attempt is still finishing, you are asked to scan the
  newest QR code, so your account never ends up expecting a code your app cannot produce.
- **A security-key confirmation now counts only for the action you confirmed** (internal development reference omitted, internal development reference omitted) — when you
  touch your security key to confirm a sensitive change, such as removing a backup email or signing
  out your other sessions, that confirmation can no longer be used for any other change, or to finish
  signing in. Where a screen never accepted a security key (regenerating backup codes), it no longer
  offers one, nor a backup code, which that screen never accepted either. Once your key confirms,
  the prompt now says so instead of still asking you to touch it. If the change is then refused, the
  prompt shows why and asks for your key again, where before it showed no reason at all.
- **Weakening your account's recovery now asks you to prove it's you** (internal development reference omitted) — turning off email or
  SMS sign-in codes, replacing your recovery key, and adding or removing a backup email now ask for
  your password, plus a code from your authenticator app or security key when you have one set up.
  Before, anyone with access to an open session could make these changes. Creating your first
  recovery key still does not ask again. After five wrong attempts in 15 minutes, further attempts
  are refused until the window passes. Separately, turning off link previews on a message now
  checks that you are allowed to manage messages before it answers, so it no longer reveals whether
  that message's previews were already hidden.
- **A dropped voice connection no longer leaves your activity visible to people you later hide it from** (internal development reference omitted) — when
  your connection to a voice channel or a direct call dropped without a clean exit, Concord removed
  you from the call but never told other people's apps. They kept showing you "in voice", and if you
  then narrowed who can see that activity, the people you excluded could keep seeing the old badge.
  Their apps now clear it as soon as Concord removes you. A clear that Concord had to retry, for
  example after a restart, is also no longer dropped when the retry comes more than 90 seconds later.
- **Identity-anomaly audit records no longer hide the request's final authorization result** (internal development reference omitted) — Concord
  now records the unusual identifier spelling as an observation, then still records whether the
  request was allowed or denied.
- **The desktop runtime now carries the current Chromium, V8, and PDFium security backports** (internal development reference omitted) — Electron was updated
  from 44.3.0 to 44.4.3, which includes upstream fixes for the five browser-engine
  vulnerabilities tracked by #3132 without changing app settings or saved data.
- **macOS installer builds no longer pull the legacy DMG dependency chain** (internal development reference omitted, internal development reference omitted) — the build now uses the existing packaging library directly, removing the vulnerable parser from build-time dependencies. The app runtime and ZIP-based auto-updates are unchanged.
- **Account safety checks now agree on one spelling of your account's identifier** (internal development reference omitted) — every
  account has an internal identifier, and the same identifier can be written in several ways
  that all mean the same account. Concord's database treats those spellings as one account, but
  two of the checks that run on every request compared them letter by letter instead — the check
  that refuses a disabled account, and the check that freezes an account while its password or
  keys are being changed. A differently-written form of the same identifier went unrecognised by
  both, so a request that should have been refused was allowed through and served as that
  account. The same mismatch reached two more places. Server moderation compared the identifier
  of the person being acted on letter by letter, so the protection that stops an owner being
  kicked, banned or timed out could be stepped around by writing their identifier differently —
  the database still resolved it to the owner. And the voice server, which checks your sign-in
  pass itself, would have seated someone under a spelling its own moderation commands could not
  address: for the rest of that call they could not be muted, removed, or have permissions taken
  away. Nothing in Concord has ever issued an identifier written any other way, so none of this
  was a way in — the checks were relying on that habit rather than enforcing it, and nothing
  would have failed if it ever stopped holding. Requests and moderation now settle on a single
  spelling before any check runs, and the voice server turns away a spelling it does not
  recognise instead of guessing. No identifier Concord issues is affected, so signing in,
  messaging and calling are unchanged.
- **Two-factor sign-in keeps its limits, and your code, through a server fault** (internal development reference omitted) — many sign-in
  code guesses sent at once can no longer get past the five-attempt limit, and a slow trickle of
  failed attempts can no longer keep two-factor sign-in locked indefinitely. A brief server fault
  no longer uses up the backup code or security-key prompt you were using, so you can try again
  once it clears. A method you set aside for account recovery can no longer be used to sign in;
  if every method you have is set aside for recovery, sign-in still asks for one of them instead
  of taking your password alone. If your security key cannot be offered at sign-in you are asked
  to try again instead of being left waiting. Retrying a slow email-code setup no longer switches
  the method back off after reporting it on. Text-message codes, which Concord cannot send yet,
  can no longer be turned on outside development builds. A server set up without email delivery
  no longer writes sign-up, account-recovery or ownership-transfer codes into its logs outside
  development; those emails fail instead.
- **Clearing a direct message or group chat's history now deletes it once everyone has** (internal development reference omitted) — clearing
  your history in a conversation has always removed it from your own view, but until now the messages
  and their attachments stayed on the server even after every member had cleared them. Once every
  current member has cleared the same history, Concord now deletes those messages and their
  attachments for good, and a group whose last member has left is cleaned up the same way. This
  happens in the background, usually within minutes; a very large conversation can take longer.

## [0.2.47] — 2026-09-18

Concord Voice now gives you clearer control over screen sharing, voice calls, and Rich Presence. Activity details show what is shared and who may receive it.

### Added

- **Changing the message timer now leaves a note in the conversation** (internal development reference omitted) — when
  someone sets, changes or turns off how long messages last, a line appears in the conversation
  saying who did it and what they chose, the same way a call leaves a record. It arrives straight
  away for everyone in the chat and stays in the history afterwards, so the answer to "when did
  this change, and who changed it?" is in the conversation rather than in someone's memory. This
  replaces a notice that each person had to dismiss separately and that vanished once dismissed.
- **Rich Presence activity now appears in member rows and profiles, and your own panel shows its eligible audience** (internal development reference omitted) — member rows show the highest-priority delivered activity with a `+N` count for additional entries, profile cards show the ordered `Now` list, and your own panel shows the confirmed audience policy for active voice or call activity. Server-delivered detail stays omitted when the server withholds it.
- **You can now pick what to share from two tabs instead of one long list** (internal development reference omitted, internal development reference omitted) — the
  share window listed every screen and every open window in two flat piles, which on a busy
  machine meant scrolling past a dozen browser windows to find the one you wanted. There are now
  Screens and Windows tabs, each a grid of thumbnails, so finding the right Chrome window takes
  one glance rather than a hunt. There was briefly a third tab, Applications, which tried to
  gather each app's windows under its name — it never reached a release, because an application
  is simply its windows, and offering the same windows twice in two different layouts asked you
  to choose between two views of one thing. Guessing which app a window belonged to also went
  wrong in a way worth knowing about: a Terminal window is titled with its size, so every
  terminal that happened to be the same size was filed together under a heading reading
  "183x62", regardless of which project it was in.
- **Screen sharing has a sound switch** (internal development reference omitted, internal development reference omitted) — sharing a screen now carries your computer's
  sound by default, and a Stream Audio control lets you turn that off if you would rather show
  video only. Worth knowing what "your computer's sound" means: it is everything playing, not
  only the screen you picked, so on a two-monitor setup the people watching hear the other
  monitor too. You can flip it mid-share without interrupting what people are watching. When you
  share a single window, what happens depends on your computer — see the next entry. Where
  single-app sound is not available the control is switched off, and the reason sits underneath
  it in plain sight rather than in a tooltip, because the one thing it will never do is quietly
  send every other sound on your computer instead. The switch stays reachable by keyboard and screen reader while it is off, so
  that explanation can be read without a mouse, and a small label beside it names what is
  actually being sent — Desktop or Off. The same applies on Linux, where sharing computer
  sound is not supported yet at all. If the call is already carrying someone else's screen
  sound, the message says so rather than leaving your share quietly silent.
- **Sharing one window can now send just that app's sound** (internal development reference omitted) — until now, sharing a
  single window meant sharing it silently. The only sound the desktop app could send was your
  whole computer's, which is the wrong answer for a window share: nobody demonstrating one
  application wants the call to hear their email arriving. On a computer that supports it,
  sharing a window now carries the sound of that app and nothing else, and the label beside the
  switch says **App** rather than Desktop so you can tell which of the two you are sending at a
  glance. Two things are worth knowing. It is per-application, not per-window — if the app has
  three windows open, you are sending the sound of all three, because that is the unit your
  operating system actually offers. And it depends on the computer, not on Concord: where the
  support is missing the switch stays off with the reason underneath it, exactly as before, and
  it still never falls back to sending everything instead. Minimising the window you are sharing
  stops the sound as well, because the app can no longer tell which window you meant.
- **You can change what you are sharing without stopping first** (internal development reference omitted) — switching from your
  screen to a document used to mean stopping the share and starting a new one, which dropped
  everyone watching and made them click back in. A Switch button now swaps the source in place.
  Anyone watching stays watching, and if you change your mind at the picker your original share
  keeps running.
- **Message expiry groundwork now covers shared chats** (internal development reference omitted, internal development reference omitted) — the server now stores shared expiry policy and expiry metadata for channels, direct messages, and group DMs, and can apply or clear that policy on existing messages in resumable batches.
- **You can now view and manage message expiration in shared chats** (internal development reference omitted) — eligible text channels, 1:1 DMs, and group DMs show the shared policy to everyone in the conversation. Who can edit follows the chat's existing permissions, and eligible participants can choose Off, 1 hour, 24 hours, 7 days, or 30 days. When setting or changing a timer, you choose whether it applies to existing messages or only new ones. Every change requires acknowledgement that deleted messages cannot be recovered. Turning the timer off lets you cancel or keep already scheduled deletions.
- **You can mute one person with a single click** (internal development reference omitted) — turning down someone whose
  microphone was picking up a television meant right-clicking their tile, opening a menu, finding
  a volume slider and dragging it to zero. Their tile now has a mute button on it. It stays
  visible while they are muted, so you can see at a glance who you have silenced rather than
  hovering over each person in turn, and unmuting returns them to the volume you had them at
  before rather than jumping to full. It is deliberately a different symbol from the crossed-out
  microphone already on the tile: that one means they muted themselves, and this one means you
  muted them, and only you hear the difference.
- **Opening a busy channel or DM drops you at your first unread message** (internal development reference omitted, internal development reference omitted, internal development reference omitted) — when there's more unread than fits on screen, the conversation now opens right where you left off reading instead of scrolled all the way to the bottom, with a Return to Latest button showing how many messages are still ahead of you. And reading messages as they arrive now actually counts: previously, only opening a conversation marked it read, so a message you read the moment it appeared could still show up as unread again after restarting the app. Now it's marked read whether you opened the conversation to see it or watched it arrive.
- **GIFs stop playing when you switch away from Concord** (internal development reference omitted, internal development reference omitted) — a chat full of
  animated GIFs kept animating while you were off in another app, spending battery and CPU on
  pictures nobody was looking at. They now stop when Concord loses focus and start again when you
  come back. Worth knowing what "start again" means, because it differs by where the GIF came
  from: one from the GIF picker resumes where it left off, while one someone attached to a
  message begins again from its first frame. That second case is not an oversight. An attached
  GIF is encrypted end to end, so the server never sees it and cannot produce a frozen frame to
  hold its place, and nothing in a browser can pause an animated image the way it can pause a
  video — so the only way to stop one is to take it off the screen, and putting it back starts it
  over. A paused attachment says so rather than going blank. The GIF picker pauses as well, since
  nobody is choosing a GIF from a window they have switched away from. This covers more than
  files that happen to end in .gif: an animated WebP, and an animated PNG that Windows or macOS
  handed us as an ordinary .png, both stop too. Neither used to, and not because anyone decided
  they should not — the file type was worked out from the label the operating system put on the
  upload, and that label cannot tell an animated WebP from a still one. Concord now looks at the
  start of the picture itself, so this works for things already sitting in your channels rather
  than only for what you upload from here on. One thing this does not
  cover, so it is not a surprise later: animated profile pictures and banners keep animating. They
  are small, they are identity rather than content, and whether "GIF playback" ought to mean
  somebody else's avatar is a separate question from the one this change answers.
- **You can choose when GIFs play** (internal development reference omitted, internal development reference omitted) — Settings ▸ Accessibility ▸
  Display has a GIF Playback control offering Auto, Always, and Hover only. Until now this rode
  entirely on Reduce Animations: turning that on made GIFs play only while you pointed at them,
  which meant there was no way to ask for hover-only GIFs without also flattening every other
  animation in the app, and no way to keep GIFs playing while Reduce Animations was on. Auto
  keeps the old behaviour and follows Reduce Animations; the other two override it in either
  direction. Nothing here ever locks — Reduce Animations is followed, not enforced — and the line
  underneath the control describes what will actually happen rather than restating the name you
  picked, because Auto and Always behave identically while Reduce Animations is off and the names
  alone cannot tell you that. This setting is stored per device; it does not follow you to your
  other computers yet.
- **Voice participants show their profile photo in the channel sidebar** (internal development reference omitted) — the
  list of who is in a voice channel showed a microphone icon and a name and nothing else, so
  telling people apart meant reading rather than glancing. Each row now carries the person's
  photo between the two, and while someone is speaking their photo picks up the same green
  their name already does. Anyone without a photo gets their initial, the way the member list
  has always shown them. The rows also line up evenly now, which they did not before: the
  plain microphone glyph and the locked-by-a-moderator version of it were different widths,
  so the column shifted depending on who was muted and how.
  This took very little to build, for a reason worth stating. The photo was already being sent
  to the app and was being thrown away one step before anything could draw it. Nothing new is
  fetched — it is the same picture the member list and the voice tiles were already showing.

### Changed

- **Message expiration and purging are no longer the same button** (internal development reference omitted) — a box
  labelled "Manage messages" sat across the top of every conversation, and behind it were two
  things that have nothing to do with each other: the timer that expires messages for everyone,
  and the tool that deletes them. Sharing one control meant it could not be right for either. It
  is gone, and the two live where each belongs.

  How long messages last is now a quiet line above the message box, next to the one telling you
  messages are encrypted — the same kind of reassurance, in the same place you already look for
  it. Everyone in the conversation sees it, whether or not they are allowed to change it, because
  knowing when your messages disappear is not a permission. It is not a button and does not want
  a click.

  Changing the timer moved to a small clock in the conversation's header, which appears only if
  you are allowed to change it, and to the right-click menu on the conversation in your list.
  Purging has its own clearly marked button in that same header, where a destructive action
  should be: labelled, not hidden behind a word like "manage".

  One deliberate restraint on the purge button — the word "Purge" is in ordinary text with a red
  icon beside it, rather than being red itself. Red text on those header colours was genuinely
  hard to read in nine of the thirty theme and light/dark combinations Concord ships, and a
  warning you have to squint at is not a warning.

  Encryption and expiry ended up as one sentence rather than two stacked lines: "Messages are
  Encrypted End-to-End and expire after 24 hours", or "and never expire" when no timer is set.
  Two short notices on two full-width rows spent space to say less, and they read as two
  unrelated announcements when they are one fact about what happens to what you are about to
  send. If Concord cannot read the timer setting at that moment it says nothing about expiry
  rather than guessing — "never expire" is a promise, and a failed lookup is not grounds to
  make one.

  The four controls in a conversation's header are now icons of equal size, each of which
  expands to show what it does when you hover or tab to it. The label floats above the header
  rather than pushing its neighbours aside, so nothing shifts under your pointer, and it waits
  a beat before appearing so that sweeping the mouse across the row does not set off four
  labels in turn. Once you have clearly stopped on one, moving to the next is immediate. If you
  have reduced motion turned on in your system settings, the label still waits that beat and
  then simply appears, with no fade or grow — the pause is there to read your intent, not to
  animate anything, so it is kept.

  Setting a timer also takes one more step than it did. Picking a duration now highlights it as
  a pending choice and waits for you to press Apply; before, a single click went straight to a
  confirmation dialog, and the only sign of which duration was already in force was slightly
  bolder text.

- **Expired messages now leave chats and pinned-message views together** (internal development reference omitted) — Concord Voice removes eligible channel, direct-message, and group-DM messages in bounded five-minute sweeps, refreshes desktop pinned-message views when a purge arrives, and clears eligible expired messages during the startup restore preflight. This does not promise instant deletion or secure erasure for clients that were offline.
- **The buttons during a call are grouped by what they do** (internal development reference omitted) — the row along the
  bottom of a call had grown to eleven buttons of equal weight in a single line, with nothing to
  say which ones affected you, which affected what you were sharing, and which would end the
  call. They are now in four groups: your own microphone, headphones and camera; everything to do
  with the screen you are sharing; the things that change your own view; and Leave, on its own.
  Three specific irritations go with it. Every button turned red when you switched it on, so a
  muted microphone looked exactly as alarming as the button that hangs up — now only Leave is
  filled red. Switch and Stop, which both act on the share you already have running, sat at
  opposite ends of the row with the Chat button between them; they are now two halves of one
  control. And the switch for sending your computer's sound was labelled "Audio Off", which reads
  as an instruction to turn audio off rather than a description of what is happening — it now
  says "Share sound", and "Sound shared" once it is on.
- **Picture-in-Picture and the stay-active setting moved into a "More" menu** (internal development reference omitted) —
  both are things you set once and forget, and neither earned a permanent place in a row you look
  at during every call.
- **Busy servers now spread voice calls across CPU cores more evenly** (internal development reference omitted, internal development reference omitted) — a voice room lives on
  one CPU core for its whole life, and the server used to hand those cores out in strict
  rotation without looking at how busy each one already was. On a server running several calls
  that meant a new room could land on the core already carrying the heaviest one while a nearly
  idle core sat next to it, and a room cannot be moved once it starts, so it stayed crowded
  until it ended. The server now gives a new room to the least busy core, counting the live
  audio and video streams each one is already carrying. When several calls start at once and
  none of them is carrying anything yet, it spreads them across cores rather than stacking
  them, which is the case that used to go wrong. Worth knowing the one case this does not
  cover: two calls that start in the very same instant can still pick the same core, which
  evens out as soon as the next call starts.

### Fixed

- **The volume controls in a voice call actually change the volume now** (internal development reference omitted) — the slider on someone's
  video did nothing at all: dragging it to 0% left them exactly as loud, and so did 200%. The
  same fault quietly took three more things with it. The master output volume in Settings did
  nothing. Quiet-user boost did nothing. And picking a specific speaker or headset did nothing —
  call audio stayed on whatever your computer treats as the default, so anyone whose default is
  their speakers has been hearing calls out loud while wearing headphones. One cause underneath
  all four: the app was reading the audio out of the hidden player it uses for each person, and
  on the current version of the browser engine that read comes back silent. Everything downstream
  of it — every volume, every boost, every device choice — was adjusting nothing while the player
  carried on at full volume. The Mute button was unaffected throughout, because it stops the sound
  before it ever reaches that point, which is why muting worked while the slider beside it did not.
- **The chat button is back in DM calls** (internal development reference omitted) — a direct message always has a conversation attached to
  it, so you should be able to talk and type at the same time, but the button never appeared. The
  call was looking for a text channel linked to a voice channel, which is how servers work and
  is not how a DM works: the conversation is the thread. The panel itself had handled DMs
  correctly since it was built; there was simply no way to open it.
- **Right-clicking someone in a DM call opens their menu again** (internal development reference omitted) — neither right-click nor the "..."
  button did anything for anyone in a direct-message call. The menu was waiting to be told which
  server the call belonged to, and a DM call does not belong to one. It now offers what makes
  sense without a server: their volume, their profile, a direct message and a friend request.
- **The profile window from a DM opens in the middle of the screen** (internal development reference omitted) — opening someone's profile
  from a DM put the window hard against the top-left corner, with its close button tucked under
  the title bar so that only the bottom sliver of it could be clicked. A site-wide styling rule
  was cancelling the one thing that centres this kind of window.
- **"Eligible audience" no longer sits under your name permanently** (internal development reference omitted) — the line explaining who can
  see your current activity took up a row of the panel at all times to answer a question you ask
  occasionally. It now appears when you point at your user area. Screen readers still announce it
  exactly as before.
- **The screen-share volume slider no longer sits on top of the video** (internal development reference omitted) — when someone shared a
  screen with sound, the slider was painted across the bottom of the picture for the whole call.
  It now appears when you point at the video and fades away when you move off.
- **Screen sharing respects your quality setting again** (internal development reference omitted) — when a screen
  share sends several quality levels at once, your chosen limit is meant to cap the largest one
  while the smaller levels get proportionally less. Instead every level was being given the full
  limit, so a share could use roughly two and a half times the bandwidth you had asked it to, and
  lowering the setting mid-share raised the smaller levels rather than lowering them. This was
  supposed to have been fixed once already; the earlier fix looked up each level by a name the
  video engine quietly replaces, so it matched nothing and changed nothing, and its test used the
  original names rather than the replacements and so never noticed.
- **Turning your camera on in a call works again** (internal development reference omitted) — switching a webcam on failed with "Could not
  start camera. Try a different camera or video preset in Settings.", and neither a different
  camera nor a different preset made any difference, because the camera was never the problem. To
  keep video watchable on slower connections the app sends your camera at three quality levels at
  once, and it was attaching a network-priority setting to all three. That setting describes the
  whole stream rather than an individual level, so the video engine refused the request outright
  and nothing was sent at all. It now rides the first level only. This is also why the failure
  looked so strange: sharing your camera alone worked, it broke the moment a second person turned
  theirs on — a second camera is what makes the app start sending three levels — and from then on
  it stayed broken even after the other person stopped, because a call keeps sending three levels
  for the rest of its life. Screen sharing carried exactly the same flaw and never showed it: it
  only sends multiple levels once at least two people are watching, which a two-person call cannot
  reach. Two related repairs come with it. Changing the video priority setting mid-call was
  silently doing nothing on any call sending multiple levels, for the same reason, and now takes
  effect. And the error message no longer blames your camera for a failure that happens well after
  the camera has already started and is working — it now says the camera started but could not be
  shared with the call.
- **The unread count on the app icon now clears when you read the messages** (internal development reference omitted) — the number on
  the Dock or taskbar icon went up when messages arrived while you were away, and then stayed
  there. It cleared only if you clicked the notification popup itself; coming back to the app,
  clicking the icon, or opening the channel in the sidebar all left it stuck, and reloading did
  not help — only quitting and reopening did. The count is now worked out from what you have
  actually read, so any way of reading a message clears it, and it is correct again the moment
  the app reloads. It also counts messages from every server rather than only the one you are
  looking at, and leaves out anything you have muted.
- **The app icon no longer bounces forever after a notification** (internal development reference omitted) — found while
  fixing the count above. Every notification asked the Dock or taskbar icon to flash for
  attention, and nothing ever asked it to stop, so it kept going until you quit. It now stops as
  soon as you come back to the app, and does not start at all if you were already looking at it.
- **Unread counts no longer leak from channels you cannot read** (internal development reference omitted) — found while
  fixing the count above. A server can let you see that a channel exists while withholding
  permission to read what was said in it. The unread totals the app fetches were worked out from
  the first permission only, so such a channel still reported a running count of how many
  messages had arrived and when — a live measure of a conversation you are not allowed to read.
  Those totals now require both permissions. The channel list is unaffected: a channel you can
  see stays visible.
- **Rotating a direct message's encryption key no longer locks everyone out of the conversation** (internal development reference omitted) — choosing Rotate Encryption Key on a DM revoked the current key without creating its replacement, so every message in the conversation showed "Unable to decrypt this message" for both people until the row was removed by hand. The rotation now creates the new key for every participant in the same step, a conversation left in that state repairs itself the next time a participant who still holds the current key opens it, and the app stops re-requesting a key the server has already said it cannot serve (it was asking a dozen times a minute for each such conversation).
- **The quieter text throughout the app is now readable** (internal development reference omitted) — timestamps,
  hints, subtitles, secondary labels and the small notices the app adds to a conversation were
  drawn in a colour that sat well below the contrast the accessibility guidelines ask for on
  body text — in some themes far enough below that it was closer to the panel behind it than to
  anything you could read. All of that text now uses the same quieter-but-legible colour the app
  already used elsewhere, so it stays visibly secondary to what people actually typed without
  disappearing into the background. Worth knowing what did NOT change: icons, chevrons, spinners
  and status glyphs keep the dimmer colour, because the guidelines ask less of a symbol than of
  a sentence and those were never the hard-to-read part. Custom themes are covered too — the
  colours they derive from your chosen background are now calculated to clear the same bar
  instead of being a fixed step away from it, which for most backgrounds was not far enough.
  That calculation also handles a pairing it used to get badly wrong: a light background chosen
  while the app is set to dark. The colour picker and the light/dark switch are independent, so
  it is an easy combination to land on, and the quieter text came out white on a white panel —
  present, but completely unreadable. It now looks for a readable colour in whichever direction
  actually has room, rather than only the one the dark setting implies.

- **Midnight Sky's quieter text is no longer too faint to read on raised panels** (internal development reference omitted) — Midnight
  Sky uses a second, softer text colour for the things that should not shout: hints, subtitles,
  secondary button labels, and the small notices the app adds to a conversation. On the darker
  raised panels that colour sat just under the contrast the accessibility guidelines ask for on
  body text, so those lines strained against the surface behind them. The colour has been
  lifted a little. It is still clearly quieter than ordinary message text — the intent was
  never to make it shout — it simply no longer sits so close in tone to the panel it is printed
  on. Midnight Sky was the only one of the fifteen schemes where this happened; the other
  fourteen already had room to spare.

- **The record of a voice call in a direct message is readable in every theme** (internal development reference omitted) — the
  "Voice call — 4:37" line that appears in a conversation after a call was drawn in a dimmed
  grey on its own tinted panel. In Midnight Sky the two were close enough in tone that the
  sentence fell below the contrast the accessibility guidelines ask for on body text, and it
  read as washed out rather than deliberately quiet. It now uses the same colour as an ordinary
  message; the tinted panel, the phone icon and the layout still mark it as a record of a call
  rather than something somebody typed. Worth knowing: the time on the right of that row is
  unchanged and is still fainter than it should be. That colour is shared with every timestamp
  in the app, so it is being corrected separately rather than in this one place.

- **A direct message containing a photo or a file now says so in your DM list, instead of reading "Encrypted message"** (internal development reference omitted) — when
  someone sent you an image, a video or a document with no words attached, the conversation list
  showed "Encrypted message" rather than telling you what had arrived. It now reads
  "Alexandra sent a Photo", and a file sent _with_ a caption shows the caption followed by the
  kind — "check this out · Photo" — so the words you were actually sent keep the space they
  deserve. The six kinds are Photo, GIF, Video, Audio file, Doc and File.

  Worth knowing why this looked fine to anyone testing it: it already worked in the conversation
  you had open on screen. It was wrong everywhere you were _not_ looking — every thread you had
  not opened, and every thread after restarting the app — which is where a message list is most
  useful. The name shown is the sender's display name, shortened if it is very long so the kind
  of file never gets cut off, and it reads "You" for your own messages. That name is drawn a
  little stronger than the words around it, so it is always clear where somebody else's chosen
  name ends and Concord's own wording begins.

  A message the app genuinely cannot decrypt still reads "Encrypted message", and it will not
  guess at a file type it has not actually confirmed. Nor will it take the sender's word for it
  where it can tell: a file uploaded as a "photo" whose actual type says otherwise is now shown
  as a plain file rather than labelled a photo on the sender's say-so. Screen-reader users now hear the preview
  line along with the conversation name, which previously was not announced at all.

  Two words changed outside your DM list as well, so the same thing is called the same name
  everywhere: a notification about an attachment in a server channel now says "Photo" where it
  used to say "Image", and "File" where it used to say "Attachment".

- **If you do drop offline, you come back on your own instead of staying offline until you reconnect** (internal development reference omitted) — the previous release stopped the drop happening in the first place; this one fixes what happened when it did. The server could tell "your presence expired" apart from "I have never heard of you" only when its own last write had failed, so an ordinary expiry was treated as the second case and you stayed marked offline for the rest of that connection. Reopening the app looked like the cure, because reconnecting was the only thing that cleared it. Now a heartbeat from a connection the server can still see restores your status, while a genuinely unknown client and anyone who chose Invisible are unaffected.

- **You no longer drop offline, or lose your connection, while Concord sits in the background** (internal development reference omitted) — leaving
  Concord open but minimised for a while could make you appear offline to everyone else even
  though you were still connected, and could quietly drop the connection itself every few
  minutes. Both came from the same cause: the desktop app sent a keep-alive every thirty
  seconds, and the operating system is free to slow that timer down once a window is hidden.
  The server now keeps both of those alive on its own schedule instead of waiting to be asked,
  so neither depends on a timer the system may pause. Worth knowing: appearing offline was the
  worse of the two, because it did not fix itself — once it happened you stayed offline to
  other people for the rest of that connection, even while you were reading and sending
  messages normally.

- **Screen-share audio loading no longer accepts a substituted JavaScript module as its native helper** (internal development reference omitted) — the
  desktop now loads the packaged audio-capture helper through the native-addon boundary directly,
  so a directory or JavaScript symlink at that path fails instead of being executed.

- **SSO sessions now survive refresh when optional session metadata is absent** (internal development reference omitted) — the control plane now reads nullable device, IP, and User-Agent fields safely, keeps Active Sessions visible, and refuses an uncorroborated grace replay quietly instead of raising a false security alert. Known stored signals still have to agree before grace recovery proceeds.

- **Chats come back to where you left them, and "Return to Latest" means it** (internal development reference omitted, internal development reference omitted) — leaving a conversation and coming back could land you a few messages above the bottom, with
  Return to Latest showing, and clicking it or scrolling down did not stick: the next visit put you
  back in the same spot. The app remembered your place as a pixel distance measured after every
  GIF had loaded, then replayed it before they had, so "the bottom" came up short by exactly the
  height those GIFs grow. It now remembers the message at the top of your view instead, which
  survives anything loading late. Leaving from the bottom forgets your place on purpose, so the
  next visit opens at the newest message and follows it as media resolves; leaving from higher up
  brings you back to that message, with Return to Latest already showing rather than waiting
  for you to nudge the scroll. Leaving a direct message with unread replies now marks the real
  number unread in the sidebar instead of always one.

- **The GIF and emoji pickers now sit against the button that opened them, with an arrow
  pointing at it** (internal development reference omitted, internal development reference omitted) — both panels used to float
  above the toolbar with a gap under them and nothing tying them to the icon you clicked, which
  on a wide window left you glancing between the panel and the row of buttons to work out which
  one you had opened. Each now measures itself and settles just above its own button with a small
  caret pointing down at it. In a window too short or too narrow to place the panel against its
  button, the panel moves to stay on screen and the caret is hidden rather than left pointing at
  the wrong thing. Both are also about 15% larger, so there is more to see without
  scrolling. The reason the gap was there is worth stating plainly: the code placed each panel
  using a fixed guess at its height that was 19 pixels too big, and had been for as long as the
  feature existed. The panels now measure themselves rather than being guessed at, so the gap
  cannot drift back.
  The GIF picker's caret is new in a stricter sense than it sounds — the code to draw one has
  been there all along, but it was being clipped away in full by the panel's own edge, and what
  survived was the same colour as the panel behind it. Nothing could catch that: the tests
  checked the caret was positioned correctly, which it was, and the tools that run them do not
  draw anything.
  On the light themes this also fixes a hairline: the GIF picker's border and caret outline were
  painted a fixed dark grey rather than following your theme, because they referred to a colour
  that was never defined anywhere. They now follow the theme like every other border.

- **Parts of the app stopped ignoring your theme** (internal development reference omitted) — twenty-three
  colours across the call banners, the outgoing-call window, the friend-picker, the category
  manager, the key-recovery prompt, the two-factor screens and the attachment preview were written
  into the stylesheet as fixed values rather than as a reference to your theme. They were chosen
  for the dark themes, so on a dark theme they looked right and nobody noticed. On any of the
  fifteen light themes they stayed dark: a near-black panel over the call controls, a charcoal
  surround on the friend-picker, a dark slab behind a call event in your messages. All of them now
  take their colour from the theme you picked. Two of them were a different kind of wrong — an
  invite embed and a direct-message profile painted their highlight in the blue Concord used to
  borrow from elsewhere, which is not the colour of anything else in the app; those now use
  Concord's own accent, which you will notice on dark themes too.

- **Buttons drawn in the accent colour keep a readable label, High Contrast included** (internal development reference omitted) — the Join
  button on an invite, the primary action on a profile card, the download link on the attestation
  warning and the active mic and chat toggles in a call all painted their label pure white on a
  background taken from the theme's accent. That reads well while the accent stays dark, which it
  is on most themes, so it went unnoticed. Switch High Contrast on and the accent becomes yellow —
  and a white label on a yellow button is close to invisible, in the one mode you would turn on
  precisely because you need to see things. The label now takes the theme's own on-accent colour,
  black where the accent is light, which is what the thirty-five other buttons in the app already
  did. The same pass caught the key-recovery prompt, whose warning text was pinned to a pale grey
  that only worked over a dark card, and the floating menu in the call controls, which was sitting
  on the page colour rather than a raised one and so had nothing but a shadow to separate it from
  the page on light themes.

- **Text on red and green buttons is legible on every theme** (internal development reference omitted) — destructive buttons
  (Leave a call, delete a category, remove a server) and the small success badges wrote their
  label as fixed white, while the button underneath took its colour from your theme. Most themes
  use a deep red, where white reads well, so this held up. Some do not: the two-factor "enabled"
  badge is green, and on the high-contrast theme that green is `#00ff00` — white on it measures
  1.37, which is not a low contrast so much as no contrast at all. The label now follows two new
  theme values, one for red fills and one for green, each picked so it stays readable against
  that theme's own shades. Green badges go from a worst case of 1.37 to 4.66. Red buttons go from
  2.43 to 3.83, which is the most a single colour can do against reds that range from near-maroon
  to salmon within one theme; that is fine for button-sized text and short of the standard for
  body text, and it is recorded rather than glossed.

  Four themes also had an accent text colour that had drifted into decoration — Cotton Candy put
  pale blue on a pale pink button, at 1.91 — and those are corrected. The wider question of
  Concord's own pink accent carrying white text, which measures 2.68, is left alone deliberately:
  that is the brand's colour and changing it is a decision about how the app looks, not a defect
  to fix in passing.

- **Server-enforced mutes look enforced again, and some hover highlights came back** (internal development reference omitted) — when a
  moderator mutes or deafens someone, the marker on that person is meant to be amber, so you can
  tell at a glance that the server did it rather than they did. In the channel list, and in the
  menu you get from right-clicking someone, that marker was being given a colour that does not
  exist, so it fell back to the same grey as everything around it and the enforced state read as
  ordinary. The same fault took the highlight off three controls that are supposed to light up
  under the pointer — the layout toggle in a call's text chat, and the two view switches — and
  removed the line under the heading of the category manager in your direct messages. All six
  were the same mistake: a colour named in the stylesheet that no theme actually defines, which
  does not fall back to something sensible but throws the whole instruction away. A test now
  fails the build if anyone writes another one of those. Colours that name a missing token but
  supply a spare are left for later: those still paint, just not the colour your theme asked
  for.

- **The documents in Settings ▸ About are set apart from one another** (internal development reference omitted) — the
  Third-party services and Legal and attribution headings are each meant to sit below a dividing
  line, and the four documents listed under the second of them — the licence, the privacy policy,
  the terms of service and the notices — are each meant to sit in a box you can open. None of
  those lines were drawn, on any theme, so the section ran together as one unbroken block of text
  with nothing to show that the documents were separate things or that they opened at all.
  Opening one made it worse: its contents ran straight on from its title with no line between
  them. All of it is now drawn, in the same colour the rest of the app uses for such lines, so it
  follows whichever theme you are on. The placeholder that stands in for a GIF you have not
  loaded yet had the same cause with the opposite symptom — its dashed outline was drawn, but
  always in one fixed dark grey. On the dark themes that passed for correct, which is why it
  lasted; on the fifteen light ones it was a charcoal rectangle on a white panel. It now takes
  its colour from the theme like everything else.

- **Video drops to a smaller size before it freezes when your computer is struggling** (internal development reference omitted, internal development reference omitted, internal development reference omitted) — Concord
  watches how long your computer takes to decode each incoming video, and when it starts falling
  behind it has to give something up. Until now it had one move: pause somebody's camera outright,
  which is abrupt and takes a person off your screen completely. It now asks the server for a
  smaller version of that video first, and keeps asking for smaller ones as long as there is
  anything smaller left; only when there is not does it fall back to pausing. On a large tile
  that means two reductions before anyone disappears, where previously there was one — though
  how many you actually get depends on your plan, because the server will only send a free
  account video up to a certain size in the first place. On a free account that is one
  reduction rather than two, and it now happens on the first attempt: previously the first
  attempt asked for a size the server was already sending, so nothing changed and you waited
  through another round of measurements before anything improved. Two further
  things worth knowing. The gentler behaviour applies only in calls where the server is already
  sending video in several sizes; in a call where it sends just one, pausing is still the only
  option and happens exactly as it did before. And where the step-down does apply, a pause that
  follows because things stayed bad now arrives several measurement cycles later than it used
  to — two per reduction, because the smaller video needs a moment to take effect before
  measuring again tells you anything true. That is roughly ten seconds per step in the opening
  half-minute of a call, and roughly a minute per step after that, once Concord settles into
  checking less often.

- **Joining a call late no longer leaves your camera stuck at one quality** (internal development reference omitted) — in calls
  where Concord sends video at several sizes, the server tells everyone once, at the moment it
  starts doing so. Anyone who joined afterwards never heard, so they sent their camera at a
  single size — which meant people watching them were stuck with whatever that size happened to
  be, whether they were looking at a small tile or a full-screen view. The same gap also meant
  that when their own computer struggled, Concord could only pause someone's video rather than
  shrink it first. Joining now tells you the current state, so a late arrival behaves exactly
  like someone who was there from the start.

- **Saving a GIF no longer sends you back to the top** (internal development reference omitted, internal development reference omitted) — clicking the save
  icon on a GIF reloaded the whole picker and lost your place in whatever you were scrolling
  through. A few seconds later it did it again, so scrolling back was not enough: you would reach
  the GIF you had just saved and be thrown to the top a second time. Both resets had one cause
  seen from two sides. Saving rebuilds the list of GIFs you have kept, and the picker treated any
  rebuild as a reason to discard what it was showing and fetch the first page again — even on the
  Trending tab, which has nothing to do with your saved GIFs. The second reset was your own save
  arriving back from the server a moment later, carrying nothing that had actually changed.
  Saving now leaves the picker where it was: the save icon fills in under your cursor, and your
  search, your tab and your scroll position all stay put. One case is still to come — removing a
  GIF while you are looking at the Saved tab reloads that list, because there its contents really
  did change.

- **Changing the screen-share quality mid-share now reaches everyone watching** (internal development reference omitted, internal development reference omitted) — a screen
  share can be sent in three qualities at once, so that someone watching in a small tile, or on a
  connection that cannot take the full thing, gets a lighter version instead of a stuttering one.
  Moving the Screen Share Cap slider, or switching Automatic Bitrate on or off, while the share was
  already running only changed the highest of the three. The two lighter versions kept whatever
  limit they were handed when the share began. Raising the cap therefore did nothing for anyone not
  already watching at full size, and lowering it to go easier on your upload did not lower theirs —
  the share ended up in a state where the number in Settings and the number actually being sent
  disagreed, with nothing on screen to say so. All three now move together, each keeping its share
  of the total the way it does when a share first starts: the number you pick is the ceiling for the
  full-size version, and the two lighter ones sit proportionally below it. Nothing about the share is
  interrupted to do it: people watching stay watching, at the size they were. Only shares sent in
  several qualities were affected — that is an H.264 and VP8 arrangement your server has to enable —
  and a share sent as a single stream was always correct.

- **Going invisible no longer knocks everyone else offline** (internal development reference omitted) — one person setting
  themselves invisible while sitting in a voice channel could disconnect every other person on the
  same server, over and over, for as long as anyone stayed connected. Each disconnect triggered the
  next one, so once it started it kept going on its own. What you saw was the app dropping its
  connection and reconnecting on a steady rhythm, with no error and nothing in the app to explain
  it, which is the part that made it hard to recognise as a single fault rather than a flaky
  network. Underneath, the server was clearing the "in a call" badge that invisible people are
  meant not to show, and when it could not work out who had been able to see that badge, it took
  the safe-looking route of disconnecting everyone so nobody could be left seeing something stale.
  The flaw was that it did this even when the person had never shown a badge in the first place —
  which is the normal case, so the safe-looking route ran constantly. It now checks whether there
  was ever anything to hide before reaching for that measure. When there genuinely is a live badge
  and the audience cannot be determined, it still disconnects, because showing your activity to
  someone who should not see it is worse than a reconnect.

- **One media server with a badly wrong clock can no longer wedge the rooms it serves** (internal development reference omitted) —
  the previous fix kept people in the participant list when a media server's clock ran slightly
  ahead. A clock that is wrong by hours rather than seconds caused something worse. Every event
  from that server carried a timestamp far in the future, and because Concord uses those
  timestamps to decide what happened in what order, every later event looked older and was
  ignored. People could not leave a call, could not be moved to another channel, and the room
  would not clear once everyone had gone. It did not fix itself when the clock was corrected
  either: the server kept counting forward from the wrong time. Concord now caps an incoming
  timestamp at the moment it actually arrives, so a wrong clock costs a little ordering accuracy
  instead of freezing the room. Direct-message calls, which had no way to recover at all, are
  repaired on upgrade. Capping the timestamp turned out to need care in one more place: a
  direct-message call records when it started and when it ended, and capping only the end left
  a call that appeared to finish before it began, which the history record refused — so the call
  simply vanished from your history with nothing shown to say so. Both ends now move together, so
  a call placed through a badly-clocked server still lands in your history with the length it
  actually ran. Capping also had to be done to the right precision, which sounds like a detail and
  was not. Concord stores these times to the microsecond, and the capped time was being measured
  more finely than that, so writing one down and reading it back gave an answer a fraction later
  than what was written. That was enough for a direct-message call with more than one person in it
  to leave the last person behind: the first person's departure stamped the other's record, and the
  other's departure then read that stamp as newer than itself and declined to act. Nobody saw an
  error, because declining was not treated as a failure, and this is the one place in Concord with
  nothing running behind it to tidy up afterwards — so that person stayed listed in the call
  indefinitely. It happened about half the time on Linux and never on a Mac, whose clock is already
  only as fine as the stored value.
- **Voice no longer drops people who are still connected** (internal development reference omitted) — when one of Concord's
  media servers reported a time slightly ahead of the real clock, usually a clock that had drifted,
  the server stopped refreshing that person's place in the room. Ninety seconds later it decided they
  had left and removed them, while their call was still running. They stayed audible to everyone but
  disappeared from the participant list, and moderator controls could no longer reach them — a
  moderator trying to remove them found nobody there. Concord now keeps refreshing each participant
  even when a media server's clock is running ahead of its own, while still letting go of people who
  really have left.
- **The controls on a video can be reached with a keyboard** (internal development reference omitted) — the buttons that
  switch a stream between the large view and the grid, step between streams, and pop a stream out
  appeared only when the mouse was over the video. Tabbing to them with a keyboard moved focus
  onto a button that stayed invisible, so anyone navigating without a mouse had no way to see
  where they were. They now appear on keyboard focus as well as on hover.
- **The share window no longer loses your place when you tab through it** (internal development reference omitted) —
  pressing Tab past the last control in the "Share your screen" window moved focus out of the
  window and onto the call controls behind it, while the window stayed open in front. Focus now
  stays inside until you choose a screen or close the window, and screen readers are told it is a
  dialog rather than an unnamed piece of the page.
- **Buttons and badges during a call follow the theme you picked** (internal development reference omitted) — several
  colours in the call controls were written as fixed values rather than taken from your theme, so
  they were tuned for one of the thirty-two light and dark schemes and merely tolerable in the
  rest. The badge shown when a server has muted or deafened someone was worse than that: the
  colour it asked for did not exist in any theme, so it quietly fell back to ordinary text and a
  server-enforced mute looked no different from a normal one.
- **Sharing a screen on a Mac now actually carries sound** (internal development reference omitted) — on macOS 14.2 and later,
  Apple requires an app to declare that it captures system audio before it is allowed to. Concord
  Voice never made that declaration, and macOS did not refuse the request or report an error — it
  handed back a silent audio track. The app could not tell the difference, so it sent that silence
  to everyone in the channel. Every Mac screen share since macOS 14.2 has been silent, with
  nothing to indicate why. The declaration is now in place. Note the floor this does not move:
  Apple exposed no way to capture system audio until macOS 13, so on macOS 12 and earlier a
  screen share carries picture only, and the app now falls back to video rather than appearing
  to send sound it never captured.
- **The app takes about 37 MB less room once installed, and no longer carries its own source code** (internal development reference omitted) — every
  release so far packed the whole desktop project inside the installed app: the source files, the
  test suite, the build scripts, the configuration used to build it, and a set of type-description
  files that only a compiler ever reads. None of it was ever read while the app was running, so it
  was pure weight. The installed app now contains only the parts it actually runs. The download
  shrinks by about 7 MB rather than the full 37 — the files that went away are text, and text
  compresses well, so they cost far more on disk than they did over the wire. Nothing about how Concord Voice behaves changes. Two details worth stating plainly
  rather than leaving to be discovered, because they are not the same thing. The build's generated
  settings file was among what shipped, and it carried no credentials — only the server address and
  feature switches already visible inside the app. Alongside it went a second settings file for our
  own internal test setup, and that one named a private address on our network. Nothing about you
  was in either file, and no password or key was ever in either, but that internal detail was ours
  and should not have travelled with the app. Neither ships now.

### Security

- **Build and deployment tools no longer carry the four affected Undici resolutions** (internal development reference omitted, internal development reference omitted) — desktop and Admin now resolve patched Undici releases across their existing major lines, and the Apple SSO and invite Workers contain Wrangler's unreleased Miniflare dependency until Wrangler ships that fix. Worker deployments now use the audited lockfile's Wrangler instead of fetching a second version at deploy time. Concord Voice runtime code did not use the affected APIs.

- **Application security events are ready for Nightwatch ingestion** (internal development reference omitted) — the control plane and media plane now emit privacy-safe, closed security records to isolated host streams. The dedicated [Nightwatch repository](https://github.com/Concord-Voice/nightwatch) owns normalization, Wazuh, provider configuration, and commissioning; this change does not modify live infrastructure.
- **Voice rooms no longer stay full after everyone has left** (internal development reference omitted) — if every final leave update was lost under queue pressure, the server could keep counting people who were no longer connected and refuse honest joins indefinitely. Concord now records a bounded lease for the last observed room state and reconciles expired participants without waiting for anyone to join elsewhere.
- **Concord now decides which signatures it trusts on release-attestation tokens, instead of letting GitHub decide** (internal development reference omitted) — before recording which app builds are genuine, the backend checks a short-lived token issued by the build pipeline. It never stated which signature algorithms it would accept, and the library it uses does not fall back to a fixed list: it adopts whatever the issuer advertises in the configuration document it publishes, which Concord fetches over the network each time the service starts. The set of trusted algorithms was therefore GitHub's to change rather than Concord's, and could have widened without any change here and without a redeploy. Concord now pins it to the one algorithm GitHub actually signs these tokens with. No token in use today is affected, and nothing about signing in, calling, or messaging changes.
- **Signing in with Google or Apple now accepts only the exact signature type those providers actually use** (internal development reference omitted) — when you sign in with Google or Apple, Concord checks a token from them that proves who you are. It confirmed the token carried an RSA signature, but not which of the three RSA strengths produced it. All three require the same private key, so this was never a way in — the door was simply wider than it needed to be. Concord now accepts the single type Google and Apple sign with, which is how it already treats its release-attestation and internal-access tokens. Signing in is unchanged.

## [0.2.46] — 2026-09-07

Voice now connects on networks that block UDP: Concord's servers were already offering relay servers that carry a call over TCP or TLS, and the desktop app was discarding that offer, so on many corporate and campus networks this was not degraded audio but no call at all. Picture-in-Picture voice windows are also hardened — each now gets a private line and a one-time credential, so nothing else loaded in the app can listen in or hang up your call — and every call stops spending part of its setup on a TCP route no Concord server has ever accepted. On the server side, updates no longer cut your connection short, and an update that comes back up unable to reach its database is now caught instead of being recorded as a success. Smaller fixes stop your voice and call status disappearing every hour, close Picture-in-Picture cleanly when it is dismissed during startup, and retire an unshared attachment when its message is deleted.

### Security

- **Voice now works on networks that block UDP** (internal development reference omitted) — Concord's servers were already offering relay servers that can carry a call over TCP or TLS when a network blocks the usual voice traffic, but the desktop app was discarding that offer and never trying them. On a corporate or campus network that blocks UDP this was not degraded audio, it was no call at all: every join attempt ran out of options and timed out. The app now uses the relay servers it is given, in the main window and in Picture-in-Picture, and falls back to today's behaviour unchanged when a server offers none.
- **Picture-in-Picture voice windows now prove who they are** (internal development reference omitted) — the private channel a PiP window uses to talk to the main window accepted instructions from any page loaded in the app and echoed every answer back to all of them. The desktop app now issues each PiP window its own one-time credential and gives it a private line, so nothing else in the app can listen in, ask for connection details, or hang up your call on your behalf.

- **Calls no longer spend part of their setup on a route that cannot work** (internal development reference omitted) — the media server told every client about a second, TCP-based way to reach it, and then held an open network connection point for that route on each call. No Concord server has ever accepted it: the door was never opened in the firewall, so the attempts were discarded in silence. Every call on every network paid for those attempts before settling on a route that works. Both the offer and the listener are now switched off unless an operator deliberately enables them. Nothing changes for people on networks that block the usual voice traffic — they keep connecting through the relay servers, which was already the route that worked.

### Fixed

- **Server updates no longer cut your connection short** (internal development reference omitted) — Concord's servers are updated many times a month, and each update restarted the part of the service that holds your connection. It was being given ten seconds to finish, while the work of closing everyone's connections tidily takes considerably longer, so it was cut off partway through every single time. Your app saw the connection vanish rather than close, showed "Reconnecting…", and if the gap ran long enough it tore down your voice call as collateral even though the voice server never restarted. The service is now given enough time to finish, and it stops accepting new connections a moment before it stops serving the ones it already has.
- **A broken update can no longer look like a successful one** (internal development reference omitted) — the check that decided whether an update had worked only asked whether the service had started, not whether it could actually reach the database it needs. A server that came back up but could not reach its database answered "healthy" while rejecting every signed-in request, and the update was recorded as a success. That check now asks whether the service can really serve, so an update that lands in that state fails and is caught instead of going live.

- **Closing Picture-in-Picture voice during startup now disconnects it cleanly** (internal development reference omitted) — closing the PiP window before voice setup finishes now cancels its pending authenticated session instead of registering it after the window is gone.
- **Your voice and call status no longer disappears every hour** (internal development reference omitted) — a routine server cleanup that clears out expired presence records was also deleting the records behind Rich Presence. The "In Voice" and "In a Call" status other people could see was wiped every time the server restarted and once every hour after that, and it did not come back until you rejoined. The cleanup now touches only the records it owns.
- **Deleting a message now also retires its unshared attachment** (internal development reference omitted) — channel messages, direct messages, and group conversations now schedule the encrypted file for storage cleanup after its final message reference is removed. An attachment shared by another message stays available until that last reference is deleted. Group deletion now revalidates the administrator under lock so a concurrent demotion or removal cannot authorize deletion.

## [0.2.45] — 2026-09-02

Voice channels and private calls can be joined again: a minimum-app-version requirement was also being applied to Concord Voice's own internal service calls, which have no app version to report, so every join attempt was refused. This release also refreshes the desktop runtime to Electron 43.5.1, keeps forwarded video within its requested temporal-quality limit, and makes sign-in and security-key enrollment reject extension data Concord Voice did not request. Groundwork for the hosted attachment-storage move continues in the background; where your attachments are stored has not changed yet.

### Changed

- **The desktop runtime was refreshed to Electron 43.5.1** (internal development reference omitted) — this updates the embedded Chromium, Node.js, and V8 versions and takes upstream sandbox, permission, custom-protocol, and context-bridge fixes without changing app settings or saved data.
- **Large encrypted attachments can now use consistently sized upload pieces during the storage rollout** (internal development reference omitted) — clients at or above the configured reader floor can choose the newer format, while older or unversioned clients stay on the earlier format and attachments continue using the existing storage destination until the R2 cutover is completed.
- **Video forwarding now keeps temporal quality at or below the requested layer** (internal development reference omitted) — the media server caps the temporal layer at the requested preference for both simulcast and scalable-video streams. When no usable spatial layer exists at or below the preferred one, it can select a higher spatial layer instead of stopping there. Upstream classifies this as a breaking behavior change; no Concord wire-format or configuration migration is required.
- **Admin and media-plane production dependencies were refreshed** (internal development reference omitted, internal development reference omitted) — `lucide-react` and the media-plane `qs` transitive moved to their reviewed releases.
- **Development and CI tooling was refreshed** (internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted) — linting, test, type, and GitHub Actions packages advanced without changing runtime behavior.

### Fixed

- **Voice channels and calls can be joined again** (internal development reference omitted) — a server setting that checks which app version you are running was also being applied to an internal check between Concord's own services, which has no app version to report. Every attempt to join a voice channel or start a call was refused as unauthorized, even when you had permission. The internal check now identifies itself directly, so the version requirement applies only where it is meant to.

### Security

- **WebAuthn ceremonies now reject unexpected client extension output** (internal development reference omitted) — sign-in and security-key enrollment explicitly fail closed when a client returns extension data that Concord did not request.

- **The control-plane dependency graph no longer includes the advisory-affected MongoDB driver release** (internal development reference omitted) — Concord does not call the affected bulk-write API, but the indirect driver is patched before that path can become reachable. The same update advances the indirect `quic-go` and `sonic` modules to their reviewed releases.

## [0.2.44] — 2026-09-01

Concord Voice now lets you choose who can see your voice-channel and private-call activity, while keeping that activity current internally, and invite previews stay tied to the invite you opened. The attachment stack is ready for the hosted storage cutover without moving writes yet: production validates the R2 destination, format rollout remains fenced to compatible clients, and cleanup work retries fairly across storage backends. Account, conversation, and server deletion now preserve the media and voice-activity cleanup they trigger, while refreshed dependencies and tooling address newly reported advisories.

### Added

- **You can now choose who sees your voice-channel and private-call activity** (internal development reference omitted) — Settings ▸ Rich Presence has separate audience and detail controls for server voice and private calls, with a preview of what each choice shares. Turning Rich Presence off hides both categories; people currently in a private call can still see that you are in their call while it is on.
- **The server can enforce a minimum desktop version before attachment format changes** (internal development reference omitted) — authenticated REST and WebSocket admission can reject clients below the configured reader floor, while public recovery routes remain available. The declaration is a compatibility signal, not attestation; stored attachment formats remain readable.

### Changed

- **Desktop utilities are now organized by concern** (internal development reference omitted) — this internal maintenance change moves existing helpers without changing app behavior, saved data, or security behavior.
- **The desktop now keeps voice-channel and private-call activity current internally** (internal development reference omitted) — incoming activity is validated by category, snapshots replace stale entries, and disconnects or server switches clear cached activity. This prepares the data path for a later visible activity view; it does not add one yet.
- **Encrypted attachment uploads now negotiate their storage format with the server** (internal development reference omitted) — when both sides support consistently sized upload pieces, the desktop uses that format; older servers keep receiving the earlier format. This prepares the hosted attachment-storage migration without changing the live storage destination yet.
- **Production now validates the R2 attachment destination before cutover** (internal development reference omitted) — production refuses to start without the configured R2 credentials even while legacy storage remains selected, retries purge work fairly across backends, and bounds object-delete timeouts. This prepares operations for cutover without moving writes yet.
- **Desktop production dependencies were refreshed** (internal development reference omitted, internal development reference omitted, internal development reference omitted) — `jose` and `lucide-react` moved to their reviewed releases, and the build-only `xmldom` lockfile resolution moved past a security advisory.
- **Admin, desktop, and media-plane development tooling was refreshed** (internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted) — linting, build, test, and type packages advanced without changing runtime behavior.
- **Browser compatibility metadata no longer resolves to advisory-affected releases** (internal development reference omitted) — the admin and media-plane lockfiles now use the reviewed metadata chain.
- **Weekly dependency triage now fails closed on missing PR CI and recognizes canonical advisory IDs** (internal development reference omitted) — type-only React update groups remain on the fast path.

### Fixed

- **Deleting a server no longer leaves its voice activity visible after a restart** (internal development reference omitted) — Concord now records the affected active voice sessions before deleting the server, so a restart cannot leave connected clients displaying stale activity after the server-side record expires.
- **Removing someone from a private conversation no longer causes a database deadlock if the administrator's account is deleted at the same time** (internal development reference omitted) — account deletion now finishes while removal safely reports changed state, because the two actions lock the affected accounts in one consistent order.
- **Removing someone from a private conversation or deleting its creator no longer leaves stale voice activity after a restart** (internal development reference omitted) — Concord now records the required activity clear before membership or account deletion removes the information needed to find it.
- **Encrypted attachments stay on the compatible storage format during rollouts** (internal development reference omitted) — the server no longer advertises a new format before every supported client can read it, and the desktop rejects capability lists containing unknown formats instead of assuming the remaining entries are safe.
- **Profile-media cleanup now survives storage failures** (internal development reference omitted) — avatar and banner uploads use immutable generation-suffixed physical keys behind stable canonical profile URLs. Durable pre-upload intents fence ambiguous writes, while unresolved intents and deletion backlog stay bounded by a per-user debt cap. Profile and friend-code avatar reads fail closed and are not cached; a late object-store write can exist briefly, but Concord Voice cannot serve it and keeps deleting it.
- **An invite preview no longer reuses details from a different invite while loading** (internal development reference omitted) — preview results are now kept with the invite code they describe, so changing codes shows a loading state until the new details arrive.
- **Prepared large encrypted attachments for consistently sized upload pieces** (internal development reference omitted) — the client and server now understand a new attachment format that reserves room for the header inside the first piece, while retaining the existing write format until compatible server and client versions can be negotiated. Attachments already stored in the older format still open normally.

## [0.2.43] — 2026-08-30

The app can reach the servers again after a certificate change locked every installed copy out. Switching between the app and the web version no longer leaves one of them unable to load your servers. Deleting your account now removes the files it uploaded, not only the record of them.

### Changed

- **Desktop state is now grouped by what it controls** (internal development reference omitted) — this is an internal maintenance change only; settings, saved data, and app behavior are unchanged.

### Fixed

- **The app can reach the servers again** — every installed copy stopped being able to reach the servers, showing "failed to fetch" when signing in, while the same account worked normally in a web browser on the same machine and on the same network. The app carried its own copy of the server's certificate details and refused to connect to anything that did not match. That certificate is replaced by our hosting provider on its own schedule, and we can neither control nor predict when — so every replacement locked everyone out until a new version of the app was built and installed. That has now happened five times. Nothing could be fixed from our side while it was happening, because the refusal took place on your own machine before any request was sent. The app now checks the certificate the same way your web browser does, which is what the web version has always done. Unexpected certificates are watched for separately, so a genuine problem raises an alert instead of locking everyone out.

### Security

- **Switching between the app and the web version no longer leaves one of them unable to load your servers** (internal development reference omitted) — if you used the app and then opened the web version at spa.concordvoice.chat, or the other way round, whichever you opened second could fail to load anything at all, while the first one carried on working. When your device asks our servers for something, the reply says which of the two is allowed to use it, and that answer is different for each. The reply did not say it depended on who was asking, so anything holding on to a copy of it — your own browser, or a network between you and us — could keep the answer meant for one and hand it back to the other, which then refused it. The reply now states that it depends on who asked, so a stored copy is only ever reused for the same one. A network that had been holding the wrong answer will stop doing so on its own; nothing needs to be cleared by hand.

- **Deleting your account now removes the files you uploaded, not just the record of them** (internal development reference omitted) — deleting an account removed every record of the files that account had uploaded, but left the files themselves in storage, and removing those records was the very thing that made the files impossible to find again. Nothing that reclaims storage could see them afterwards, so they stayed indefinitely. Your profile picture and banner are held unencrypted, because the server resizes them for you — so those in particular outlived the deletion meant to remove them. What needs removing is now noted before the account record goes, and deleted once the deletion has definitely gone through. A separate daily sweep reclaims attachments left behind by deletions that already happened. Profile pictures from those earlier deletions cannot be found automatically, and clearing them still needs to be done by hand — that has not happened yet.
- **Changing a server's owner no longer leaves voice activity visible under the old permissions** (internal development reference omitted) — ownership changes every permission at once, but people already connected were not rechecked when a transfer completed, expired, or was reversed. A transfer could also finish from stale information after the server had changed owner again. Ownership changes now recheck active voice access immediately, and stale transfers are cancelled inside the same locked transaction instead of overwriting newer ownership.

## [0.2.42] — 2026-08-29

Interface improvements reach you again without waiting for an app update. Invite links now arrive, open, and leave the server where you can see it, and the GIF picker's Categories and Recent tabs work for the first time. Someone who has just lost access can no longer be shown that you are online, and the screen-capture protection switch no longer reports itself as on where nothing is enforcing it.

### Fixed

- **Interface improvements reach you again without waiting for an app update** (internal development reference omitted) — the app can be given interface fixes without you installing a new version, but the compatibility check that decides whether to accept them had become far stricter than it needed to be. Everyone was quietly being served the older copy built into their installed app instead, losing every interface fix since their version was released, with nothing to indicate it had happened. The check now asks what the interface actually needs rather than assuming it needs the newest possible app.
- **"Open in Concord" on an invite link no longer does nothing** (internal development reference omitted) — the invite was handed to the app and then dropped whenever the app reloaded its own interface, which it does by itself after a network problem. The button appeared to do nothing at all, with no way to tell that anything had been lost. An invite is now held until you have actually been shown it.
- **An invite that arrived while you were offline now appears when you return** (internal development reference omitted) — invites sent as a direct message were only ever collected while you were looking at your direct messages, so one that arrived while you were away sat unseen until you happened to open that view. It now reaches you without your going looking for it.
- **A server you join from a link now appears in the sidebar straight away** (internal development reference omitted) — joining through an invite left the server missing from your sidebar until you switched servers or restarted, and its unread badge missing for the same reason. There are two ways to join a server and only one of them was recording it; both now do.
- **The GIF picker's Categories and Recent tabs now show something** (internal development reference omitted) — both tabs have been empty for everyone since they appeared. Categories were being looked for in the wrong part of the reply and every one was discarded; Recent was never told which account it belonged to, so nothing was ever recorded for it to show. Both now fill.
- **The GIF picker no longer shrinks when a tab has nothing in it** (internal development reference omitted) — the picker sized itself to whatever it was showing, so moving to an empty tab collapsed the window and moving back grew it again. It now keeps its size.
- **A burst of reconnections no longer holds up messages** (internal development reference omitted) — working out who is allowed to see your online status takes several trips to the database, and those were made on the same queue that delivers messages and direct messages. When many people reconnected at once — after a restart, or a network problem — that queue waited and everything else waited behind it. The lookups now happen alongside it rather than in front of it.

### Security

- **Someone who has just lost access can no longer be shown that you are online** (internal development reference omitted) — working out who is allowed to see your online status takes several trips to the database, and that answer could be worked out just before someone's access was taken away and then delivered just after it had gone. Blocking someone, removing a friend, leaving or being removed from a server, having a server deleted around you, and deleting your account were all affected. The answer is now discarded and worked out again whenever access was being changed while it was being calculated.
- **Screen-capture protection no longer reports itself as on where it cannot be enforced** (internal development reference omitted) — the setting can only be enforced on macOS and Windows. Elsewhere it was accepted, saved, and reported back as active while nothing was actually stopping a capture. It now answers honestly on every platform. A saved preference that cannot be read — a damaged or unreadable settings file, rather than one that was never written — is now treated as off and reported as such, instead of being quietly assumed.

## [0.2.41] — 2026-08-27

A large release. Attachments can finally be as big as your plan has been advertising, you can choose who may send you a friend request, and server roles can be reordered. Several long-standing voice and video faults are fixed — on recent builds of the engine Concord is built on, encrypted audio and video were being handed to the decoder still encrypted, and picture-in-picture windows had never decrypted at all.

### Added

- **Attachments can now actually be as large as your plan says** (internal development reference omitted) — the size shown for your plan has been right for a while, but sending anything near it failed. Files were encrypted in one piece, and the layer in front of Concord refuses a single upload over 100 MB. Large files are now encrypted and sent in pieces, so the advertised size — up to 256 MiB — is reachable. An upload abandoned partway is now cleaned up rather than leaving the space consumed.
- **You can choose who may send you a friend request** (internal development reference omitted, internal development reference omitted) — Settings ▸ Privacy now offers _everyone_, _people I share a server with_, or _nobody_, and where you have chosen not to receive them the Send Friend Request button is not offered. Someone who tries anyway is told the same thing, in the same time, as they would be if you had blocked them or if you did not exist — so your setting cannot be worked out by testing against it.
- **Server roles can be reordered** (internal development reference omitted) — Server Settings ▸ Roles can now be reordered by dragging, by keyboard, or from the toolbar, and the new order is committed with an explicit **Apply Order** rather than saving as you move. It works for anyone allowed to manage roles rather than only the server owner, and you can only move roles below your own highest one.

### Changed

- **Concord now trusts more than one interface-signing key at a time** (internal development reference omitted) — Concord keeps a verified copy of its own interface, so a network problem drops you back to the last good version instead of an older one built into the app. That copy is only trusted when it was signed with a key baked in at the time your copy of Concord was built, and until now exactly one key was accepted. Changing that key would therefore have silently disabled the cache for everyone who had not yet updated, with no way to tell them otherwise. Concord now accepts a list of keys, so the outgoing and incoming key both work while a changeover is in progress and nobody loses the cache in between.

### Fixed

- **Voice and video are no longer garbled on recent builds** (internal development reference omitted) — a change in the browser engine Concord is built on stopped decryption being applied when it was set up a moment after a stream had started. Encrypted audio and video were handed to the decoder still encrypted: noise instead of speech, a black picture instead of video. Decryption is now set up as the stream is created, and where it genuinely cannot be applied the stream is refused rather than played back as noise.
- **Picture-in-picture windows now decrypt what they show** (internal development reference omitted) — a picture-in-picture window never applied decryption at all, so microphone, camera and screen share were all fed to it still encrypted — garbled audio and a black picture, every time, for as long as the window has existed. It now decrypts, and closes the stream rather than showing noise if it cannot.
- **The connection to Concord no longer drops every few minutes** (internal development reference omitted) — connections through the hosted service were closing abnormally every few minutes and reconnecting within half a second. Brief, but voice could be torn down along with it. The connection is now kept alive properly, and a voice session is no longer ended by a disconnection that recovers inside its grace period.
- **One cause of a voice server disconnecting everyone at once is fixed** (internal development reference omitted) — when a voice server's internal queue overflowed it disconnected every client on that machine rather than the one session responsible. That case no longer does. Worth stating plainly: this is one cause removed, not the behaviour eliminated — the same over-broad disconnect is still reachable by other routes, and those are still being worked through.
- **Mentions in direct messages show names again** (internal development reference omitted) — a mention inside a direct message showed a raw identifier instead of the person's name. Names are now resolved from the people in that conversation, and the raw form appears only while the conversation is still loading.
- **A restart no longer leaves your activity showing after you have left** (internal development reference omitted) — if the server restarted between your leaving a voice channel or a call and that being sent out, nothing durable recorded that the clear was owed, so others kept seeing you there until a ninety-second timeout ran out. That clear now survives a restart.
- **A storage fault no longer reveals which friend codes exist** (internal development reference omitted) — the page behind a friend code answers every invalid code identically on purpose, so guessing at codes tells you nothing. While the avatar store was unwell it answered differently for a real code than an invented one — enough to sort guesses into hits and misses. It now answers the same way in both cases.

### Security

- **An opt-in setting can stop Concord's windows being captured** (internal development reference omitted) — on macOS and Windows you can now ask the operating system to leave Concord's main and picture-in-picture windows out of screen recordings and screenshots. It is off unless you turn it on, and it is not offered on Linux, where it cannot be enforced. Packaged builds also switch off several Electron capabilities that are only useful for inspecting a running application.

## [0.2.40] — 2026-08-20

Friend codes now have a web page of their own. Opening someone's friend link in a browser shows who it belongs to and offers to open Concord, and the page looks the same whether the code is live or not, so nobody can use it to work out which codes exist.

### Added

- **Friend code links now open a real page, and open the app** (internal development reference omitted) — a friend link shared outside Concord used to lead nowhere useful. It now opens a page showing the username, display name and avatar of whoever the code belongs to, with a button that opens Concord straight to the request. Codes that have expired, been revoked, or been used up show a neutral "code unavailable" page — deliberately the same size and shape as a live one, so someone guessing codes cannot tell from the page which guesses landed.

### Fixed

- **Clicking several friend or invite links quickly no longer loses the ones in the middle** (internal development reference omitted) — links arriving close together were collapsed to the newest, so clicking three in a row opened only the last. All of them now open, in the order you clicked, one per second. A repeat of the link already on screen is still ignored, since re-opening it would change nothing.
- **A friend link clicked before signing in no longer follows you into someone else's account** (internal development reference omitted) — a link clicked while signed out was held and opened after signing in, which is intended, but it was never cleared when you signed out. On a shared computer the next person to sign in saw it. Signing out now discards anything held.
- **A brief server problem no longer reports a working friend code as dead** (internal development reference omitted) — if the server was briefly busy or unwell, the page said the code was no longer valid and that answer was remembered for a minute, outliving the problem. The page now distinguishes "this code is not valid" from "we could not check right now", and recovers within seconds.
- **Revoking a friend code now tells you when it fails** (internal development reference omitted) — if revoking failed — offline, signed out, or a server error — nothing was shown and the code stayed listed, so you could believe a still-live code had been withdrawn. Revoking is the only way to take a friend code's public page offline, so the failure is now reported.

### Fixed

- **Leaving, being kicked from, or being banned from a server now stops your activity showing there immediately** (internal development reference omitted) — until now, someone who left or was removed from a server could keep seeing what its members were doing in voice for up to a minute and a half, and a member who had just joined saw nothing until the next thing happened. Both are fixed: the people who can see you are worked out and updated at the moment membership changes. Being kicked or banned now signs you out on every device you have open rather than refreshing what you can see, so nothing stale is left behind. Someone who can still see you another way — a second shared server, or through friends — is unaffected and is not cleared by mistake.
- **Deleting a server, or deleting your account, no longer leaves your activity visible to others** (internal development reference omitted) — the information needed to work out who could see you used to be erased before it could be used, so people kept a stale view. That information is now captured first and cleared afterwards. Deleting your account also reaches people connected to other servers in the cluster, which it previously did not. One limit worth stating: when a **server** is deleted, a member who is offline at that moment may still see a stale custom status from it until they next reconnect — live voice activity clears within about ninety seconds either way.

### Changed

- **A friend or privacy change can briefly delay saving your custom status** (internal development reference omitted) — accepting or removing a friend, blocking someone, redeeming a friend code, or changing who can see you through friends-of-friends now updates your custom status for everyone who can see it, and does so durably rather than only for people currently connected. While one of those is being applied, your own **Settings ▸ Presence** save may return "service unavailable" for up to 30 seconds, with the wait shown in a `Retry-After` header. That is expected rather than a fault: both writes touch the same presence state, and they are serialized so neither can publish an audience that is already out of date. Retrying after the indicated delay succeeds.

## [0.2.39] — 2026-07-31

Pointing Concord Voice at a self-hosted server is now checked at the moment it connects, not only when you type the address. A server on your own network or machine is reachable only after you approve it in a confirmation the web layer cannot draw or click.

### Fixed

- **A compromised web layer can no longer make the app read your private network** (internal development reference omitted) — when you pointed Concord Voice at a self-hosted server, the app checked that the address looked well-formed but never checked where it actually led. That let a hostile page inside the app reach devices on your own network, or your machine itself, and read what came back. Addresses are now checked at the moment the connection is made, addresses that could never host a server are refused outright, and a server on your own network or machine is reachable only after you have approved that server in a confirmation the web layer cannot draw or click. Approving a server is also now a separate, deliberate step rather than something a successful connection did on its own.

## [0.2.38] — 2026-07-30

Encrypted channel keys now reach every member of a busy server without stalling, retrying forever, or handing out a key that was already revoked. The DM and server sidebars share one adaptive layout, so both behave the same way when you narrow the window. Several sign-in paths were tightened so an abandoned or superseded attempt cannot disturb the session that replaced it.

### Changed

- **The DM and server sidebars now share one adaptive layout** (internal development reference omitted) — both sidebars resize by the same rules, so a narrow window behaves the same whether you are in a server or in direct messages.
- **Updated the voice server and interface libraries** (internal development reference omitted, internal development reference omitted, internal development reference omitted) — the media server moves to mediasoup 3.22.0 and the interface moves to the current React release.

### Fixed

- **Encrypted channel keys are delivered in batches** (internal development reference omitted) — a channel with many members no longer sends one key request per member at once. Distribution is grouped, so a large server finishes key delivery instead of overwhelming the server and stalling.
- **Key distribution can be cancelled while it waits to retry** (internal development reference omitted) — when the server asks the app to slow down, the app waits before trying again. Leaving the channel during that wait now cancels the work instead of letting it resume against a channel you left.
- **Key recovery resumes after a repeated rotation** (internal development reference omitted) — two rotation batches arriving for the same channel could leave a member waiting for a key that was never re-requested. Recovery now wakes on the second batch.

### Security

- **Signing up with a password after backing out of a social sign-in no longer costs you your encryption keys** (internal development reference omitted) — if you started signing in with Google or Apple and then changed your mind, the abandoned attempt kept a hold on the app's secure-key slot. A password registration that followed silently failed to save its encryption keys to your keychain, so those keys did not survive a restart until you logged in again. The abandoned attempt is now released before registration begins.
- **Two-step verification codes are compared in constant time** (internal development reference omitted) — the comparison no longer stops early on the first wrong character, so the time it takes to reject a code reveals nothing about how much of it was correct.
- **A key from a revoked epoch is refused** (internal development reference omitted) — the server rejects distribution into a channel epoch that has already been revoked, so a removed member cannot be handed a key that a rotation was meant to take away.
- **Manual channel rotation is fenced to the current credentials** (internal development reference omitted) — a rotation started before a password change or key reset can no longer commit afterward.
- **A channel's active key epoch survives concurrent writes** (internal development reference omitted) — the recorded epoch is read from the durable ledger, so two overlapping rotations cannot leave the channel pointing at the wrong one.
- **Signing out other sessions no longer signs out the one you are using** (internal development reference omitted) — revoking all sessions preserves the session that issued the request.
- **Rich Presence suppression fails closed** (internal development reference omitted) — if the server cannot confirm that your activity should be hidden, it hides it rather than publishing it.
- **Returning from invisible does not replay a stale suppression** (internal development reference omitted) — a suppression queued while you were hidden is discarded when you come back online, so your activity is not blanked immediately after you choose to show it.
- **Resolved static-analysis findings in authentication handling** (internal development reference omitted) — no user-visible behavior changed.

## [0.2.37] — 2026-07-27

Sign-in and session handling were hardened across the board: a stolen or superseded token can no longer outlive the reset that was meant to end it, and an account that is disabled while connected is disconnected instead of lingering. Recovery keys now use the stronger elliptic curve the rest of Concord Voice already required.

### Fixed

- **Desktop updates no longer risk a crash or missing preload files** (internal development reference omitted) — updated Electron to 43.1.1, which fixes failures when an installed app archive is replaced while Concord Voice is still running and includes Chromium 150.0.7871.114.
- **Encrypted key requests no longer flood the server** (internal development reference omitted) — the number of key requests the app sends at once is bounded, so rejoining a large server does not produce a burst the server must shed.
- **Bulk message removal is more reliable** (internal development reference omitted) — purging a member's messages records its progress, so an interrupted purge can be identified and completed instead of stopping silently partway.

### Security

- **Linking a social account is bound to the password you just verified** (internal development reference omitted) — if your credentials change between entering your password and completing the link, the link is refused instead of completing against the old proof.
- **Token-theft revocation and session creation cannot interleave** (internal development reference omitted, internal development reference omitted) — when a reused refresh token triggers a revocation, a sign-in running at the same moment can no longer slip a new session past the sweep.
- **A disabled account is disconnected from an open connection** (internal development reference omitted) — the live connection checks account state rather than trusting the token it was opened with, so disabling an account takes effect immediately instead of at the next reconnect.
- **Recovery keys use P-384** (internal development reference omitted) — account-recovery key agreement now enforces the same curve Concord Voice requires everywhere else. Weaker curves are rejected.
- **Going invisible or offline clears activity that was already published** (internal development reference omitted) — voice and call activity is suppressed on the transition itself, so nothing you were doing stays visible after you hide.

## [0.2.36] — 2026-07-26

Desktop actions that touch your camera, clipboard, updates, or permissions now verify the request came from Concord Voice itself before acting.

### Security

- **Desktop actions now reject untrusted web content** (internal development reference omitted) — screen capture, clipboard, update, permission, lifecycle, and developer controls now act only on calls from a permitted Concord Voice frame. Floating call windows remain usable while the app switches between remote and bundled interface sources.

## [0.2.35] — 2026-07-25

Windows updates install when you click Restart. The previous installer deleted its own files partway through and stopped, which is why updates appeared to do nothing.

### Fixed

- **Windows updates now install when you click Restart** (internal development reference omitted) — after downloading an update, clicking Restart closed Concord Voice and then nothing happened; a window flashed briefly and disappeared. Running the downloaded installer by hand did the same thing, and the only way through was to move that file somewhere else, like your Downloads folder, and run it from there. Updates now install normally from the Restart button, with no manual step. Leftover files from the old installer are tidied up on a later launch, and your downloaded updates and your installed app are never touched by that cleanup.

## [0.2.34] — 2026-07-25

Version 0.2.33 never reached you — it was bumped but never released, so everything it carried ships here. On top of that: signing in with Google or Apple is bound to the account state at the moment it completes, and the "can't reach Concord servers" warning clears itself once the connection is up.

### Fixed

- **The "can't reach Concord servers" warning at startup now clears itself** (internal development reference omitted) — this warning could appear when the app opened and then stay on screen even though you were connected and your messages were loading, only going away if you manually refreshed the window. It now disappears on its own the moment the app reaches the servers, and it no longer appears at all when the connection was already up. Startup warnings that are not about reachability — a configuration the app declined to use, or an app version too old for the current interface — still appear as before, and now say which of those actually happened instead of blaming the connection.

### Security

- **Social sign-in is bound to your current account state** (internal development reference omitted) — the session is created while the account record is held, so a password reset or key recovery running at the same moment cannot leave a sign-in behind that the reset was meant to cancel.
- **Updated the desktop router to close a published advisory** (internal development reference omitted) — resolves GHSA-QWWW-VCR4-C8H2.
- **Encryption key self-healing checks the recipient's current key** (internal development reference omitted) — when the app re-sends a channel key to someone who missed it, it names the key version it wrapped for. The server refuses the delivery if that person has since reset their keys, so a key is never wrapped to an identity that no longer exists.

## [0.2.33] — 2026-07-24

This release was bumped but never published; its contents reached you in 0.2.34. It is the largest security pass of the beta so far. Sessions, encryption keys, and password changes are now tied to the exact credentials that authorized them, so a stale sign-in, a slow reply, or an interrupted password change can no longer disturb the session that replaced it.

### Fixed

- **Key recovery stays reachable after a password sign-in** (internal development reference omitted) — the app holds the main route until encryption is ready, so you are not dropped into the app in a state where recovery cannot be started.
- **Custom Status visibility settles consistently** (internal development reference omitted) — the record of who may see your status is reconciled durably, so a failed update is repaired rather than left half-applied.
- **Rich Presence cleanup resumes after an interruption** (internal development reference omitted) — if hiding your activity fails partway, the evidence of what still needs hiding is kept and retried instead of being derived from state that has already moved on.
- **Private calls appear only once both sides are admitted** (internal development reference omitted) — a call that is still being authorized no longer shows up as a live call or leaves a phantom entry behind if it is abandoned.
- **Hidden presence stays hidden across servers** (internal development reference omitted, internal development reference omitted, internal development reference omitted) — presence state is synchronized rather than recomputed per connection, malformed stored activity is repaired instead of trusted, and a superseded presence generation is verified before it is allowed to write.
- **Reduced complexity in sign-in and voice lifecycle code** (internal development reference omitted) — no behavior change; the paths involved in the fixes above were made easier to reason about.

### Security

- **Two-step verification during a social sign-in completes inside the app's protected process** (internal development reference omitted) — your credentials stay tied to that exact sign-in, so a stale or superseded attempt cannot overwrite a newer session. The main window also never unlocks before encryption is ready for the account that just signed in.
- **Encryption keys are committed by the sign-in that created them** (internal development reference omitted) — a sign-in that has been replaced can no longer clear or overwrite the keys belonging to the one that replaced it.
- **A password change that succeeded is never reported as a failure** (internal development reference omitted) — if the server confirms the change but the reply cannot be read, the app signs you out and asks you to sign in again rather than continuing with keys that no longer match your password.
- **Key delivery checks the recipient against a concurrent key reset** (internal development reference omitted) — a channel key is not wrapped to someone whose keys were reset while the delivery was in flight; the delivery is skipped and re-queued against their new key.
- **A slow rejected request cannot end a rotated session** (internal development reference omitted) — a rejection that arrives after your session has already been refreshed is ignored instead of signing you out.
- **Grace-period session refresh follows an exact lineage** (internal development reference omitted) — when two devices refresh at nearly the same moment, the replacement session is matched to the exact token it replaced rather than guessed from timing.
- **Every session is bound to the credentials that authorized it** (internal development reference omitted, internal development reference omitted) — a password change or key recovery advances a per-account marker, and any sign-in still holding the previous marker is refused. This closes the window where a session authorized just before a reset could be created just after it.
- **Password changes rotate every encrypted setting together** (internal development reference omitted) — saved GIFs, interface preferences, friend organization, and Custom Status visibility are re-encrypted in one transaction. Previously a failure partway could leave some of them locked to your old password.

## [0.2.32] — 2026-07-22

Sign-in and session refresh stay with the account and server you chose, even when two sign-ins overlap or you switch between hosted servers.

### Fixed

- **Sign-in and refresh stay with the right account and server** (internal development reference omitted) — session rotation, simultaneous sign-ins, and switching between hosted servers can no longer mix credentials or continue a stale sign-in against the newly selected server, and a superseded sign-in no longer leaves its encryption keys resident in memory.

### Changed

- **Updated the voice server and build libraries** (internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted) — mediasoup, Vite, the icon set, and the media server's request parser move to their current releases.

## [0.2.31] — 2026-07-22

Groundwork for showing what your friends are doing. The server can now publish voice and call activity, and the desktop app validates it without displaying it yet.

### Changed

- **Privacy-safe voice activity groundwork** (internal development reference omitted) — the server can now publish authorized Server Voice and Private Call activity, and the desktop safely validates the new wire format without displaying it yet. A later client update will add the visible experience.

## [0.2.30] — 2026-07-18

Reconnecting after a network drop now restores what changed while you were away, instead of leaving stale servers and missing messages on screen. Moderators can remove a member's messages when kicking or banning them, and Apple sign-in works again on desktop.

### Added

- **Removing a member can remove their messages** (internal development reference omitted) — kicking or banning a member can also purge the messages they posted in that server.
- **Account activity metrics and an Admin workspace** (internal development reference omitted) — operators can see aggregate account activity. The figures are counts only; they carry no user, server, or channel identity.
- **Safe rotation of the two-step verification encryption key** (internal development reference omitted) — operators can rotate the key that protects stored two-step secrets. Each stored secret records the key version that sealed it, so old and new keys coexist during a rotation.

### Fixed

- **Reconnecting restores what changed while you were disconnected** (internal development reference omitted, internal development reference omitted) — a brief network drop used to leave the app showing servers you had been removed from and missing messages sent during the outage, until you reloaded. Reconnecting now refreshes your memberships and back-fills the open conversation. Your unsent drafts and queued messages are kept, which the previous behavior discarded.
- **Apple sign-in completes on desktop** (internal development reference omitted) — the sign-in returns through the hosted bridge rather than a local address, which the previous flow could not reach.

### Security

- **A password change that loses your encryption keys fails closed** (internal development reference omitted) — the app signs you out and asks you to sign in again rather than continuing in a state where your keys no longer match your password.
- **Encryption keys are not cleared by a sign-in that has ended** (internal development reference omitted) — a sign-in torn down mid-setup can no longer clear the keys belonging to the sign-in that replaced it.
- **Security headers are sent once, from one place** (internal development reference omitted) — duplicate and conflicting headers are removed, and the transport-security policy is stated once.

## [0.2.29] — 2026-07-15

Moderators can now remove a member's messages in bulk, across a channel, a server, or a conversation. Voice gets more honest about video: the codec shown in Settings is the one actually in use, and H.264 calls are encrypted per access unit rather than whole-frame. Operators get an Admin console with aggregate figures that carry no user identity.

### Added

- **Bulk message removal** (internal development reference omitted) — a moderator can delete a member's messages across a channel, a whole server, or a conversation in one action. The removal is recorded as counts and context only; message text is never written to the audit record.
- **The Admin Portal console** (internal development reference omitted) — operators get a web console for instance health and aggregate metrics, behind its own sign-in.
- **Rich Presence category settings persist** (internal development reference omitted) — your per-category choices for what activity is shared are stored, so they survive a restart.

### Fixed

- **The codec shown in Settings is the one in use** (internal development reference omitted) — the video settings screen previously showed the codec the app intended to use. It now separates what will be tried from what is actually running, read from the live connection, so hardware encoding is never claimed when software encoding is doing the work.
- **H.264 video is encrypted per access unit** (internal development reference omitted) — H.264 calls use a format-aware encryption boundary instead of whole-frame encryption, which lets the video server route frames without ever seeing their contents. HDR target selection was corrected at the same time.
- **Admin metrics read correctly** (internal development reference omitted, internal development reference omitted, internal development reference omitted) — hourly averages stay inside their bounds, chart values are formatted and labelled, and host telemetry is restored in production.

### Security

- **Desktop release signing is isolated from pull requests** (internal development reference omitted) — pull-request packages now run unsigned without signing secrets or OIDC, while signed releases rebuild protected `main` with environment-scoped credentials and same-run artifacts.
- **Voice activity is authorized before it is published** (internal development reference omitted) — the server checks that a viewer is allowed to see your Server Voice or Private Call activity, and publishes the minimum needed to render it.

## [0.2.28] — 2026-07-14

Direct-message call previews now say what actually happened, and Custom Status counts characters the way you would.

### Fixed

- **DM call previews describe the call** (internal development reference omitted) — a call entry in your conversation list now reflects its outcome instead of showing one generic line for every case.
- **Custom Status counts characters, not bytes** (internal development reference omitted) — the length limit counts Unicode code points, so accented letters, non-Latin scripts, and emoji no longer consume several characters each.

### Changed

- **Desktop build and test tooling refreshed** (internal development reference omitted, internal development reference omitted, internal development reference omitted) — updated tsx, Vitest coverage tooling, ESLint React tooling, and TypeScript ESLint behind the desktop build and test pipeline.

## [0.2.27] — 2026-07-14

Activity History arrives: an opt-in, self-only record of your own voice and call intervals, off until you turn it on and deleted on your schedule. Video codec selection was also reworked so a call that drops to software encoding re-selects instead of staying there.

### Added

- **Activity History — opt-in and self-only** (internal development reference omitted) — you can record your own voice and call intervals and review them in Settings. It is off until you consent, the record is visible only to you, and it is pruned on the retention window you choose. Consent is versioned: if the terms change, you are asked again rather than opted in silently. Nothing is recorded while it is off.
- **A read-only admin metrics API** (internal development reference omitted, internal development reference omitted) — operators can read aggregate instance metrics through a restricted role. The figures are fixed numeric counts with no user, server, or channel dimension.

### Fixed

- **Video re-selects its codec mid-call** (internal development reference omitted) — when a call falls back to software encoding, the app now re-selects toward a hardware-capable codec instead of staying on the slower path for the rest of the call. Re-selection is serialized so two triggers cannot fight, and cannot oscillate between two software codecs.
- **Chat stays where you left it** (internal development reference omitted) — a late layout change no longer scrolls you away from the newest message.
- **Edited encrypted messages display correctly** (internal development reference omitted) — an edit is decrypted before it is stored, so an edited message no longer shows as unreadable.
- **Dialog placement corrected** (internal development reference omitted) — the outgoing-call and What's-new dialogs no longer appear behind other windows.

### Security

- **Safer desktop update checks** (internal development reference omitted) — updated the parser used by Concord Voice's desktop updater to prevent specially crafted recovery-feed YAML from consuming excessive CPU during update checks. Routine security maintenance; no action needed.

## [0.2.26] — 2026-07-12

Screen sharing was rebuilt around what viewers can actually see. Every participant can share at once, each stream carries its own volume and mute, and the server sends each viewer only the resolution their window needs. Video codec selection now learns from the live call whether hardware encoding is really being used.

### Added

- **Everyone can share their screen at once** (internal development reference omitted, internal development reference omitted) — concurrent screenshare limits were raised substantially, and a shared screen is sent at several qualities so each viewer receives the one that fits their window instead of everyone paying for the largest.
- **Per-stream screenshare audio** (internal development reference omitted) — each shared screen has its own volume slider and mute. Muting one stops the server sending you that audio at all, rather than silencing it locally.
- **A voice tile view** (internal development reference omitted) — participants fill the available space, and the view switch moved somewhere you can find it.
- **Custom Status recipient exceptions** (internal development reference omitted) — you can hide your Custom Status from specific people. The exception list is encrypted with your password-derived key, so the server enforces it without being able to read it.

### Changed

- **Subscriptions expire on schedule** (internal development reference omitted) — when a plan reaches the end of its period, entitlements return to the free tier automatically instead of waiting for the next sign-in.

### Fixed

- **Hardware video encoding is detected from the live call** (internal development reference omitted, internal development reference omitted) — the app probes what your machine can encode and then confirms it against the running call, so a codec that claims hardware support but silently falls back is demoted for the session.
- **Voice survives a brief server blip on join** (internal development reference omitted) — joining a call during a short interruption now retries instead of failing outright.
- **Picture-in-picture windows close promptly when a call ends** (internal development reference omitted) — leaving voice now releases each floating window's media immediately instead of leaving an always-on-top window visible while cleanup requests time out.
- **System audio is captured only for whole-screen shares** (internal development reference omitted) — sharing a single window no longer captures audio from everything else.
- **The focused screen stays focused** (internal development reference omitted) — tuning into a second screen no longer displaces the one you were already watching.
- **Update settings are quiet at rest** (internal development reference omitted) — interface and app update state are reported separately instead of one status standing in for both.
- **Renamed Settings ▸ Sounds and Notifications to Notifications** (internal development reference omitted).
- **Screen quality demand no longer flaps for AV1 and VP9 shares** (internal development reference omitted) — layer preferences for those codecs apply directly instead of feeding the decision about whether to publish several qualities.
- **Follow-up fixes from the desktop notification work** (internal development reference omitted) — remaining issues found while auditing the notification changes.

### Security

- **Voice publishing is checked by the media server** (internal development reference omitted) — permission to speak or share is enforced where the media is actually accepted, not only in the interface.
- **An empty server-supplied voice identity fails closed** (internal development reference omitted) — following the change in 0.2.25, a blank authoritative name is used as-is rather than falling back to the name the client supplied.
- **Channel video limits follow the server's plan, not the owner's** (internal development reference omitted) — per-room camera and screenshare limits are resolved from the server's own subscription, so a premium member or owner on a free server cannot raise the limits for that room.
- **Screenshare resolution and frame rate follow your plan** (internal development reference omitted) — the limit is a combined pixel-rate, so 1080p30 and 720p60 both work on the free tier while 1080p60 does not.
- **Shutdown no longer strands connections** (internal development reference omitted, internal development reference omitted, internal development reference omitted) — the server drains in-flight requests before closing live connections, and voice transports are released by their owner.

## [0.2.25] — 2026-07-09

A full security audit of channel and message permissions landed: what you can see is now checked on every path that can reveal it, not only on the one that lists it. The message composer also grew up — emoji autocomplete, keyboard shortcuts for the pickers, and no more layout jump on first open.

### Added

- **Emoji autocomplete in the composer** (internal development reference omitted) — type a `:shortcode:` and pick from suggestions inline.
- **Keyboard shortcuts for the pickers** (internal development reference omitted) — Ctrl+E opens emoji, Ctrl+G opens GIFs, from the composer.
- **Tune in and out of individual screens** (internal development reference omitted) — you choose which shared screens to watch, globally or one at a time.
- **See exactly what a bug report sends** (internal development reference omitted, internal development reference omitted) — a preview shows the diagnostic log attached to a report before you send it. Identifiers are replaced with per-report placeholders, and tokens, addresses, and file paths are removed.

### Fixed

- **The composer no longer jumps when a picker first opens** (internal development reference omitted, internal development reference omitted) — picker code is loaded ahead of the first open, so the composer stops expanding and snapping back.
- **Emoji-only messages render large again** (internal development reference omitted) — a message that is only `:shortcode:` emoji scales up, matching the behavior for literal emoji.
- **Privacy and Security settings no longer sign you out** (internal development reference omitted) — a background settings sync that failed used to be treated as an expired session.
- **Dialogs trap focus correctly** (internal development reference omitted) — modals announce themselves to screen readers, keep keyboard focus inside while open, and return focus where it came from.

### Security

- **Channel visibility is enforced everywhere it can leak** (internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted) — channel metadata, unread counts, attachments, encryption keys, live update broadcasts, and voice participant lists are all gated on permission to view the channel, rather than on server membership alone. Previously a member of the server could learn about channels they could not open.
- **Removed members are evicted from live connections** (internal development reference omitted, internal development reference omitted) — kicking or banning a member closes their live subscription immediately, and voice counts are scoped to servers the recipient is actually in.
- **Group conversation creation respects your privacy settings** (internal development reference omitted) — you cannot be added to a group conversation by someone your settings do not permit to message you.
- **Channels cannot be bound across servers** (internal development reference omitted) — a channel category must belong to the same server as its channel, enforced by the database rather than by the caller.
- **Sign-in state is consumed atomically** (internal development reference omitted, internal development reference omitted) — single sign-on state and one-time tokens are read and deleted in one step so they cannot be replayed, and device attestation tokens are bound to the account that minted them.
- **Voice display identity comes from the server** (internal development reference omitted) — the name and avatar other participants see are resolved from your authenticated account rather than sent by your app, so a modified client cannot present itself as someone else in a call.
- **Admin enrollment links no longer carry their token in the address** (internal development reference omitted) — the enrollment token is kept out of the URL, where it would be recorded in history and server logs.
- **A wildcard origin is refused in production** (internal development reference omitted) — the server refuses to start with a permissive cross-origin setting, which would have allowed a hostile page to make credentialed requests.
- **Archive handling hardened against malicious files** (internal development reference omitted) — updated the tar library used by our desktop build tooling and the voice server's installer so maliciously crafted archives (compression bombs and malformed headers) can no longer hang or crash those steps. Routine hygiene — no user action needed.
- **Verification failures respond consistently** (internal development reference omitted) — running out of verification attempts returns the same shape as any other failure, so the response reveals nothing extra.

## [0.2.24] — 2026-07-05

A batch of things that were quietly broken: username search, the friend-request sound, password-reset email, and the verification-code window that was too short to type in. Voice also picked up a visual layer that shows who is speaking and that the call is encrypted.

### Added

- **A voice stage that shows who is talking** (internal development reference omitted, internal development reference omitted) — the active speaker takes the foreground, an encryption ring marks the call as end-to-end encrypted, and a backdrop appears before you join. Messages that fail to decrypt are now visibly distinct rather than blank.
- **Show your password while typing it** (internal development reference omitted) — the sign-in screen has a reveal toggle.

### Fixed

- **A clearer signal when your encryption keys can't be saved** (internal development reference omitted) — if your device keychain is locked or full when Concord saves your encryption keys for next launch, the app now notices instead of failing silently. Your current session keeps working either way; if saving didn't succeed, signing in again on the next launch restores it.
- **Searching for a user by name works again** (internal development reference omitted).
- **Friend requests make a sound again** (internal development reference omitted).
- **Password-reset email is delivered** (internal development reference omitted) — the reset message failed to send; it now arrives.
- **There is time to enter the email verification code** (internal development reference omitted) — the window was short enough that the code often expired while you were reading it.
- **One-to-one calls show a single bottom bar** (internal development reference omitted) — the duplicate bar is gone.
- **Settings and help overlays close** (internal development reference omitted) — both can be dismissed without reaching for the keyboard.
- **A GIF that fails to load no longer signs you out** (internal development reference omitted) — a rejection from the third-party GIF service is no longer read as your session expiring.

### Security

- **The test environment setting is refused in production** (internal development reference omitted) — the server will not start in production with test-mode configuration, which relaxes several checks.

## [0.2.23] — 2026-07-04

Your subscription is visible and manageable in the app, and call quality, upload limits, and server capabilities follow the plan you are on. When bandwidth gets tight, Concord Voice sheds webcam before screenshare before audio, so voice stays clear.

### Added

- **Manage your subscription in the app** (internal development reference omitted) — a new Settings ▸ Subscription page lets you redeem a code and see your current plan and status at a glance.
- **Audio and video quality that follows your plan** (internal development reference omitted, internal development reference omitted, internal development reference omitted) — call quality, per-room camera and screenshare limits, and server capabilities now track your tier. When bandwidth gets tight, the app sheds webcam before screenshare before audio, so voice stays clear.
- **Higher limits for subscribers** (internal development reference omitted) — how many servers you can create and how deep you can search now scale with your subscription.
- **Smoother GIFs, safely** (internal development reference omitted) — the GIF pipeline preserves animation while guarding against maliciously oversized files.

### Fixed

- **Single sign-on no longer times out mid-signup** (internal development reference omitted) — SSO registration now gets a proper 15-minute token window and recovers gracefully if it expires, instead of stranding you partway through.

### Changed

- **Behind-the-scenes CI, supply-chain hardening, and dependency upkeep** (internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted) — tighter least-privilege on the release workflows, a security-patched build toolchain, and routine dependency refreshes.

## [0.2.22] — 2026-07-02

Update Concord Voice and the next launch tells you what changed. This file became the canonical public record at the same time, and the release pipeline now refuses to cut a version without an entry for it.

### Added

- **See what changed, right after you update** (internal development reference omitted) — update Concord Voice, and the next launch shows a "What's new" dialog covering every version since the one you had. It shows once, works offline, and never slows startup. Read it or dismiss it — your call.
- **A changelog you can hold us to** (internal development reference omitted) — this file is now the canonical public record of every change we ship. Every release since Beta is documented below, and CI refuses to cut a new version without its entry. No entry, no release.

## [0.2.21] — 2026-07-02

Windows verifies an update's signature before installing it, and public releases carry build provenance you can check yourself. A batch of chat fixes came with it: pinning works again, scroll stays where you left it, and previews say what they mean.

### Added

- **Bigger image uploads for subscribers** (internal development reference omitted) — avatar, banner, and server-icon size limits now scale with your subscription tier.

### Changed

- **Behind-the-scenes tooling, CI, and documentation upkeep** (internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted).

### Fixed

- **Server message pinning works again** (internal development reference omitted) — and pinned media now stays inside its panel.
- **No more scroll jumps** (internal development reference omitted) — switching channels quickly no longer snaps you back to a stale position.
- **Previews that speak plainly** (internal development reference omitted) — DM and notification previews now say "Photo" instead of a raw placeholder.
- **The composer menu renders where it belongs** (internal development reference omitted) — above the placeholder text, not behind it.
- **The client asks for less** (internal development reference omitted, internal development reference omitted) — removed permission checks the client was never entitled to make. Fewer requests to the server, less noise on the wire.
- **One path for update checks** (internal development reference omitted) — the app's UI and binary update checks now share a single, consistent path.
- **Cleaner sign-up hint** (internal development reference omitted) — the suggested identity no longer carries a stray "@concordvoice.chat" suffix.

### Security

- **Windows verifies every update before it installs** (internal development reference omitted) — the update manifest now names the expected publisher, so Windows checks the code signature of each downloaded update at install time.
- **Proof, not promises, for public releases** (internal development reference omitted) — every publicly mirrored desktop release asset now carries verifiable build provenance. You don't have to take our word for what's in the installer — verify it.
- **Attestation misconfiguration now fails loud** (internal development reference omitted) — corrected the verification default and added a guard so a misconfigured deployment cannot pass silently.

## [0.2.20] — 2026-06-30

Self-hosted instances get the full feature set with no artificial limits. The licence, privacy policy, and terms are one click away in About.

### Added

- **Self-host it, get everything** (internal development reference omitted) — run Concord Voice on your own hardware and every premium entitlement is unlocked out of the box. No subscription required. That is the self-hosting deal: your server, your rules, all of it.
- **Legal documents, one click away** (internal development reference omitted) — the license, terms, and other notices are readable directly from Settings ▸ About.

## [0.2.19] — 2026-06-30

You can sign in to a Concord Voice server you run yourself. Clients that lost track of the hosted interface can now find their way back without reinstalling.

### Added

- **Sign in to your own server** (internal development reference omitted) — the first slice of self-hosted support: desktop login can now route to an operator-run Concord Voice server instead of the managed service.

### Fixed

- **Stranded clients can find their way home** (internal development reference omitted) — the app now checks the public release feed for updates, so a client stuck on an old version can always recover and get current again.

### Changed

- **Behind-the-scenes deployment and tooling upkeep.**

## [0.2.18] — 2026-06-29

Certificate pinning is back on for connections to Concord Voice servers.

### Security

- **Certificate pinning restored** (internal development reference omitted) — our certificate pinning for Cloudflare-fronted connections had lapsed. This release brings it back: connections to Concord Voice services are once again verified against the exact certificates we expect. We are telling you it lapsed because you deserve to know — that is the point of this file.

## [0.2.17] — 2026-06-29

React to direct messages with emoji. Connecting at launch is more reliable, and rotating a certificate no longer locks you out of your own server.

### Added

- **React to direct messages** (internal development reference omitted) — DMs now take emoji reactions, the same way server channels already do.

### Changed

- **Behind-the-scenes tooling, CI, and dependency upkeep** (internal development reference omitted).

### Fixed

- **You connect reliably, right from launch** (internal development reference omitted) — we removed a race in the WebSocket authentication handshake that could leave the client sitting disconnected just after startup.
- **Certificate rotation no longer locks you out** (internal development reference omitted) — clients enforcing the old production API certificate pin failed to connect after we rotated the certificate. The client now trusts the rotated pin.

## [0.2.16] — 2026-06-29

Choose how much a notification reveals on screen. Every colour scheme is now free for everyone, and opening direct messages no longer interrupts your call.

### Added

- **Choose what your notifications reveal** (internal development reference omitted) — a new setting controls how much message content appears in desktop notifications, so nothing private shows up on a shared or public screen unless you want it to.
- **Custom themes are now free** (internal development reference omitted) — build your own theme, no subscription required.

### Fixed

- **Your call keeps playing when you open DMs** (internal development reference omitted) — the voice audio pipeline stays mounted while you browse direct messages, so call audio no longer drops mid-navigation.
- **macOS relaunches after installing an update** (internal development reference omitted) — "Restart to update" on macOS quit the app and then didn't bring it back. The update-install restart path now reopens Concord Voice for you.

### Changed

- **Behind-the-scenes tooling, CI, and supply-chain upkeep** (internal development reference omitted, internal development reference omitted) — we retired a legacy automation credential and automated the guard on our supply-chain indicator-list refresh. Nothing changes for you; the pipeline gets safer.

## [0.2.15] — 2026-06-29

Moderators can time a member out instead of removing them. Passkey registration is stricter, and erasing your account now revokes the session that asked for it.

### Added

- **Member timeout moderation** (internal development reference omitted) — moderators can put a member in timeout for a set duration. Participation is restricted until the clock runs out — no permanent ban required for a temporary problem.
- **macOS Applications-folder move prompt** (internal development reference omitted) — the first time you launch Concord Voice from outside /Applications, it offers to move itself there. One click gets you a proper install and updates that land reliably.

### Changed

- **Behind-the-scenes tooling, CI, and deployment upkeep.**

### Security

- **Stricter passkey registration requirements** (internal development reference omitted) — WebAuthn registration now demands passkey-grade platform authenticator options. Every new passkey you create meets the security bar we set — no silent downgrades.
- **Access token revoked on account erasure** (internal development reference omitted) — erase your account and your current access token dies with it. No authenticated session outlives the deletion.

## [0.2.14] — 2026-06-28

A channel can set one audio standard for everyone in it, so voice quality no longer depends on each person's plan. Usernames became case-insensitive everywhere, and self-hosted certificate failures are reported instead of swallowed.

### Added

- **Per-channel audio quality standard** (internal development reference omitted) — set one audio quality tier on a voice channel and every member gets it, bounded by the server's tier. A new slider in channel settings puts the choice where you'd expect it.
- **SVC / Simulcast casting toggles** (internal development reference omitted) — advanced video controls in Settings ▸ Audio & Video let you decide which layered-video codec modes your client publishes with.
- **Self-hosted TLS certificate provisioning** (internal development reference omitted) — self-hosting? One script provisions your origin certificate: self-signed, Let's Encrypt, or bring your own.

### Changed

- **My Profile moved into Settings ▸ Account** (internal development reference omitted) — profile editing now lives in Settings, where the rest of your account already is; the old quick-link deep-links to the same place. The SSO controls on the Security page got a polish pass too (internal development reference omitted).
- **Behind-the-scenes tooling, CI, and dependency upkeep** — supply-chain threat-list refreshes, deploy-summary and mirror-sync fixes, and dev-environment maintenance.

### Fixed

- **Username case-handling consistency** (internal development reference omitted) — usernames are now case-insensitive everywhere. This closes three real bugs: an SSO mixed-case lockout that blocked profile edits, duplicate accounts differing only by case, and friend-add lookups that failed on capitalization.
- **macOS notification permission status** (internal development reference omitted) — Concord Voice now reconciles its notification settings with the actual macOS permission state, so what you see in Settings matches what your Mac will do.
- **Self-hosted TLS failures no longer silent** (internal development reference omitted) — local certificate provisioning errors were being swallowed. Now they're reported to you, the operator, plainly — a failure you can see is a failure you can fix.

### Security

- **Hardened email MFA setup for production** (internal development reference omitted) — tightened the email-based multi-factor enrollment flow for production.
- **Cached-UI origin allowlisted ahead of activation** (internal development reference omitted) — the signed offline UI cache's origin is now on the server allowlists before the cache goes live, so passkey login and voice keep working the moment that fallback activates.

## [0.2.13] — 2026-06-28

Invite bubbles name the person who actually invited you. Linux updates are signature-verified before they install, and the signed offline interface cache went live.

### Fixed

- **Invite bubbles now name the person who actually invited you** (internal development reference omitted) — Send-to-a-Friend invite messages in chat were attributed to the wrong user. Now the sender shown is the sender who sent it.
- **System Permissions settings are easier to find and understand** (internal development reference omitted) — clearer wording and better navigation for the System Permissions section in Settings, so you know exactly what the app can touch.

### Security

- **Linux updates are signature-verified before they install** (internal development reference omitted) — AppImage, deb, and rpm update artifacts now carry detached Ed25519 signatures, verified against a public key bundled in the client. An update that fails verification does not install. No exceptions.
- **Signed offline UI cache is live** (internal development reference omitted, internal development reference omitted) — when the latest UI can't be fetched, the desktop client falls back to a last-known-good copy that is cryptographically verified against an embedded public key before it runs. You get a working app; you never get unverified code.

### Changed

- **Behind-the-scenes tooling, CI, and configuration upkeep** (internal development reference omitted, internal development reference omitted) — removed a retired sign-in configuration value from internal checks and documentation.

## [0.2.12] — 2026-06-27

Your camera and screen share stay live when the call switches video quality layers, instead of dropping for a moment each time.

### Fixed

- **Your camera and screen share stay live when video quality layers switch** (internal development reference omitted) — re-negotiating video layers mid-call could stop the underlying capture track and take down every camera in the room. Producers now keep the track alive across the switch.
- **Age-verification status survives a reload** (internal development reference omitted) — verified state is re-fetched from the server on startup, so if you've already verified, you won't be asked again.

### Changed

- **Behind-the-scenes tooling, configuration, and code-quality upkeep** (internal development reference omitted, internal development reference omitted, internal development reference omitted) — engineering write-ups, groundwork for a storage-configuration rename, and static-analysis cleanup. Nothing changes in how the app behaves for you.

## [0.2.11] — 2026-06-26

Hardware video encoding is offered only when your graphics hardware actually supports it.

### Fixed

- **Hardware video encoding now matches what your GPU can actually do** (internal development reference omitted) — Concord Voice now queries your system's supported hardware encode profiles instead of assuming a fixed codec set, so camera and screen-share encoding picks codecs your GPU genuinely accelerates.

## [0.2.10] — 2026-06-26

You can run your own Concord Voice instance. AV1 video no longer black-screens in end-to-end encrypted calls.

### Added

- **Run your own Concord Voice instance** (internal development reference omitted) — a guided installer walks you through standing up a self-hosted instance, and a new `concord-selfhost` command starts, stops, monitors, and health-checks the stack.

### Fixed

- **AV1 video no longer black-screens in end-to-end-encrypted calls** (internal development reference omitted) — AV1 camera and screen-share video could fail to decrypt, leaving you staring at a black screen. We reworked per-frame media encryption (frame crypto v4) so AV1 streams decrypt reliably — encryption stays on, and your video stays visible.

### Changed

- **Behind-the-scenes tooling, CI, and dependency upkeep** (internal development reference omitted, internal development reference omitted) — refreshed our supply-chain threat indicators and taught macOS release builds to retry a transient DMG packaging flake.

## [0.2.9] — 2026-06-24

Video no longer black-screens after an encryption key rotates mid-call, and the offline interface cache is signed so a tampered copy cannot load.

### Added

- **Signed offline UI cache** (internal development reference omitted) — the desktop app keeps a cryptographically signed last-known-good copy of the latest UI. If the update server is briefly unreachable, you still start with the current interface — verified before it loads, not just cached.
- **Server capabilities discovery endpoint** (internal development reference omitted) — a new `GET /api/v1/server/capabilities` endpoint tells your client what a server — including a self-hosted one — supports before you connect.

### Fixed

- **Video no longer black-screens after a mid-call key rotation** (internal development reference omitted) — encrypted video frames now carry their channel-key version, so receivers select the correct decryption key after an end-to-end-encryption key rotation. Your video stays up; the encryption stays on.
- **Public release mirroring now triggers only on real releases** (internal development reference omitted) — we corrected the condition that publishes signed builds to the public downloads page. It fired when it shouldn't have; now it fires exactly when a release ships.

### Security

- **Stricter validation of the API server address in packaged builds** (internal development reference omitted) — the app now accepts only the official service address or a self-hosted server that passed the app's own verification probe. Hardens the client against a compromised UI steering it somewhere else.

### Changed

- **Behind-the-scenes tooling and engineering-workflow upkeep** (internal development reference omitted, internal development reference omitted).

## [0.2.8] — 2026-06-24

Voice and video got steadier: audio devices switch mid-call, direct-message calls open the call view with sound, and encrypted video keeps up with layered streams. Older direct-message attachments decrypt correctly again.

### Changed

- **A cleaner invite landing page** (internal development reference omitted) — open a server invite link and you land on a restyled page that gets you into the server without ceremony.
- **Behind-the-scenes tooling, CI, and test upkeep** (internal development reference omitted, internal development reference omitted, internal development reference omitted) — including expanded automated color-contrast (WCAG) checks, so accessibility regressions get caught by machines, not by you.

### Fixed

- **UI updates reach you reliably, and sessions survive a soft reload** (internal development reference omitted) — the client retries fetching the latest UI after a cold-start network hiccup and checks for newer UI on its own (waiting until you're out of a call). Session-only sign-in — "Remember Me" off — now keeps your credentials and encryption keys across an in-app reload, held in memory only. They never touch disk.
- **Encrypted video stays in sync with layered and simulcast streams** (internal development reference omitted) — we hardened the end-to-end-encryption key epochs for media, so camera and screen-share video no longer desyncs or black-screens when the stream is layered.
- **Switch audio devices mid-call** (internal development reference omitted) — change your microphone or speaker during a call and the active audio streams retarget correctly. No rejoin required.
- **DM voice calls open the call view and play remote audio** (internal development reference omitted) — answer a direct-message call and the voice view comes up with the other person audible, reliably.
- **Older DM attachments decrypt correctly** (internal development reference omitted) — images and files in your DM history now use the right key context after key rotations, so your encrypted history stays readable to you.
- **Your DM list shows accurate online status** (internal development reference omitted) — presence in DM rows now stays in sync with your friends list.
- **Image lightbox polish** (internal development reference omitted) — enlarged images are properly centered and no longer show a magnifier cursor.

## [0.2.7] — 2026-06-23

> Versions 0.2.4–0.2.6 never reached you — they were release-pipeline iterations that produced no public release. Everything they carried ships here.

Images open in a proper lightbox, and invite links can be public. Camera quality adapts in group calls, and video recovers on its own once an encryption key catches up.

### Added

- **Image lightbox viewer** (internal development reference omitted) — click any image attachment to see it full size, zoom in, and save it with a native Save As dialog.
- **Public invite links** (internal development reference omitted) — share a server invite link anywhere; it opens a landing page and routes new members straight into the app.
- **Smarter camera quality in group calls** (internal development reference omitted) — SVC-first camera layer selection means the server sends each viewer the resolution they actually need. Your bandwidth goes to the video you're watching, not the video you aren't.

### Fixed

- **Video recovers after encryption key catch-up** (internal development reference omitted) — end-to-end-encrypted video used to black-screen after a key epoch desync. It now recovers on its own; you don't have to do anything.
- **Audio output device changes apply mid-call** (internal development reference omitted) — switch your speaker or headset in settings and live call audio follows immediately.
- **Large batch of chat and UI polish** (internal development reference omitted–internal development reference omitted) — DM conversation avatars, DM unread badge and notification suppression while a conversation is open, self-sent DM previews, GIF embeds in DMs, self-mention highlighting, ordered-list numbering, inline code wrapping, bigger click targets for the message composer and user popovers, context-menu and screen-share-picker layering fixes, notification-sound and TTS preview feedback, theme-correct self profile card, friend category manager styling, voice participant frame overlap, active server restored after reload, and blocked partially-uploaded attachment sends.
- **Linux packaging hardening** (internal development reference omitted, internal development reference omitted, internal development reference omitted) — Concord Voice launches more reliably across Linux desktops, menu icons render correctly in every desktop environment, and RPM packages keep their sandbox permissions intact.
- **Media uploads report clearly when unavailable** (internal development reference omitted) — when media storage is disabled, the server now says so with a proper "service unavailable" response instead of a confusing error. You deserve a straight answer, even from an error message.

### Security

- **Closed a WebSocket channel permission bypass** (internal development reference omitted) — channel access rules (roles and channel-level permissions) were enforced on the REST API but not consistently on the real-time WebSocket path. They now hold on both. Stating it plainly: this was a gap, and it's closed.

### Changed

- **Behind-the-scenes tooling, CI, and dependency upkeep** — routine dependency updates (including Electron 42.4.1 and mediasoup 3.20.9), release-pipeline and deployment reliability work, and infrastructure housekeeping.

## [0.2.3] — 2026-06-22

The message composer shows how many characters you have left, and handles going over the limit without losing what you typed.

### Added

- **Live message character-limit counter with overflow handling** (internal development reference omitted) — see exactly how much room you have left as you type. The composer counts characters against your account's message limit and flags over-limit text before you send — no more surprise rejections.

### Changed

- **Behind-the-scenes tooling, CI, and documentation upkeep** (internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted) — build-pipeline and security-scanning maintenance, documentation cleanups, and a routine supply-chain threat-list refresh. Nothing changes in how Concord Voice behaves for you.

## [0.2.2] — 2026-06-22

Premium features show what they unlock and can be redeemed with a code. Voice audio crackle is fixed, and reconnecting after a server deploy no longer needs a restart.

### Added

- **Premium lock UI + redemption codes** (internal development reference omitted, internal development reference omitted, internal development reference omitted) — the groundwork for subscriptions: lock badges and gates mark premium features, a universal redemption-code system redeems access, and per-tier audio/video quality limits are enforced server-side when you join voice.
- **Public Known Issues list** (internal development reference omitted, internal development reference omitted) — a pinned, always-current Known Issues tracker in the public feedback repo, linked straight from the in-app feedback dialog. Check whether we already know about your issue before you file it.
- **"Load latest UI" recovery** (internal development reference omitted) — a Settings ▸ About button, plus an automatic launch-time retry, that loads the newest app UI without a restart. If a slow network start left you on the built-in fallback, you're one click from current.
- **Admin/ops console authentication** (internal development reference omitted) — operator console sign-in requires a password plus a WebAuthn hardware-key second factor. A password alone doesn't open the ops door.
- **GIFs load automatically by default** (internal development reference omitted) — new accounts start with "Load GIFs from KLIPY automatically" turned on. Existing accounts keep the choice you already made — your setting is yours.

### Fixed

- **Voice audio crackle and garbling** (internal development reference omitted, internal development reference omitted) — small calls no longer clip the first words of speech: the speaker-limit cap now stays out of the way in rooms it was never needed in, and silent (DTX) audio frames pass through correctly instead of garbling the start of speech.
- **Reconnect after server deploys** (internal development reference omitted) — a brief server-side disconnect now reconnects on its own instead of stranding you at "Reconnecting", and a transient sign-in hiccup no longer wipes a "Remember Me" session. "Remember Me" now means it.

### Changed

- **Behind-the-scenes tooling, CI, documentation, and dependency upkeep** (internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted) — feedback-triage automation, release-mirror pipeline fixes, a security-list refresh, doc cleanups, and audio diagnostics that are opt-in and off by default — diagnostics run only when you turn them on.

## [0.2.1] — 2026-06-20 (macOS DMG installer delivery)

> Patch release. v0.2.0-Beta shipped the macOS client as a `.zip` only — the
> signed-and-notarized `.dmg` installer was built, signed, and notarized by CI
> but never attached to the GitHub Release because the release-asset glob
> omitted `*.dmg`. This release corrects the release pipeline so the branded
> drag-to-Applications `.dmg` ships alongside the `.zip` (which remains the
> electron-updater auto-update artifact — the `.dmg` is install-only).

Friend categories also arrive: organize your direct messages into groups with your own emoji and colours. The grouping is encrypted with your key, so the server stores it without being able to read it.

### Fixed

- **Friend categories in direct messages** (internal development reference omitted) — group your direct messages into categories with your own emoji and colours. The grouping is stored encrypted with your key, so the server keeps it without being able to read it.
- **macOS `.dmg` installer now attached to releases** (internal development reference omitted) — the release-asset `find` glob in `build-desktop.yml` (feeding `gh release create`) omitted `*.dmg`, so the notarized installer was missing from the v0.2.0 GitHub Release and the public mirror. The DMG is now attached and normalized to `ConcordVoice-<version>-macos-<arch>.dmg`, consistent with the `.zip`. The `latest-mac.yml` auto-updater manifest deliberately remains `.zip`-only (Squirrel.Mac cannot auto-update from a DMG).

## [0.2.0-Beta] — 2026-06-20 (Phase 2 — Beta release)

> Release-level rollup of Phase 2A + Phase 2B work. Per-revision detail lives in the `[0.1.12]`–`[0.1.18]` entries below; this entry surfaces the user-visible themes that close the v0.2.0-Beta milestone.

The Beta release. Sign in with Google or Apple, add two-step verification or a passkey, and recover an account you have locked yourself out of. Roles and permissions arrive alongside GIFs, server moderation, and end-to-end encrypted attachments.

### Added

- **Federated identity — Google SSO** (internal development reference omitted) — backend OAuth flow for Google sign-in and registration; desktop client integration shipped alongside Apple SSO in internal development reference omitted (`client/desktop/src/main/ssoLoopback.ts`, `Login.tsx` / `Register.tsx`).
- **Federated identity — Apple Sign in with Apple** (internal development reference omitted) — privacy-relay-aware Apple SSO alongside Google.
- **MFA / WebAuthn authentication** (internal development reference omitted) — TOTP, WebAuthn/FIDO2, backup codes, recovery circles, trusted devices; closes #89.
- **Account erasure (GDPR right to be forgotten)** (internal development reference omitted) — `POST /api/v1/privacy/erase-account` wired to a transactional account-deletion service that cascades across all linked tables; `refresh_tokens.user_id` cascades atomically.
- **Account recovery** (internal development reference omitted) — zero-knowledge key recovery flow.
- **RBAC / SBAC permissions system** (internal development reference omitted) — granular role-based access control with audit logging; closes #82. Context menu wired to roles in internal development reference omitted.
- **Object storage on MinIO** (internal development reference omitted) — user image assets migrated from PostgreSQL to S3-compatible storage with two-tier media access; closes #166.
- **Server ownership transfer** (internal development reference omitted) — full lifecycle with MFA, email confirmation, reversal tokens; closes #244.
- **Email verification on registration** (internal development reference omitted) — SMTP-based verification; later migrated from Proton SMTP to Resend with branded templates and `verify.concordvoice.chat` subdomain.
- **Pending registrations** (internal development reference omitted) — registration creates `pending_registrations` with a 15-minute TTL; closes #527 and #621.
- **Chat enhancements (#168 series)** — message reactions (internal development reference omitted), reply / quote (internal development reference omitted), pinning (internal development reference omitted), E2EE-native search (internal development reference omitted), file & image attachments (internal development reference omitted), draft persistence (internal development reference omitted), desktop notifications (internal development reference omitted), keyboard shortcuts (internal development reference omitted), group DMs (internal development reference omitted), extended Markdown rendering (internal development reference omitted).
- **Klipy GIF integration** (internal development reference omitted) — GIF search, picker, and privacy proxy through the control-plane; theme-aware logos and disclaimers.
- **Server-enforced mute and deafen** (internal development reference omitted) — server-side mute/deafen state propagated to the mediasoup SFU; consumers paused/resumed at SFU level.
- **@mention notification routing in E2EE channels** (internal development reference omitted) — server-side mention detection without decryption.
- **DM key-epoch enforcement** (internal development reference omitted) — key revocation table and epoch checking in the WebSocket path; closes #122.
- **Channel-key rotation on member removal** — E2EE forward secrecy for channel keys; closes #96.
- **Desktop auto-updater** (internal development reference omitted, internal development reference omitted–internal development reference omitted) — safe updates with rollback, branded splash screen, fill-progress, error states, position memory, and structured logging.
- **Server-proxied desktop updates** (internal development reference omitted) — privacy-first update delivery with no per-client telemetry.
- **SPA deployment pipeline** (internal development reference omitted) — file server, versioning, and GitHub Actions workflow for hot-update SPA bundles. SPA deploy contract added in internal development reference omitted coupling bundle-hash and handler-path.
- **Bundled-SPA fallback (Option C)** (internal development reference omitted, internal development reference omitted, internal development reference omitted) — desktop client falls back to the bundled SPA on hot-update failure with the `app://` scheme, an Option C user-facing overlay, and IPC v9.
- **WebSocket reconnect race fix + subscribe-barrier protocol** (internal development reference omitted) — closes #752.
- **Self-hosted coturn STUN/TURN** (internal development reference omitted) — infrastructure for NAT traversal; cert isolation and `turn.concordvoice.chat` SAN added in internal development reference omitted.
- **Public Tier-1 media proxy** (internal development reference omitted) — unauthenticated access for public media assets.
- **Token theft detection** (internal development reference omitted) — machine ID + IP binding with automatic revocation. Sessions capture real client IP via trusted-proxy CIDR allowlist in internal development reference omitted.
- **Proactive token refresh** (internal development reference omitted) — main-process JWT refresh before expiry; closes #240 and #254.
- **Profile and identity asset theming** (internal development reference omitted, internal development reference omitted) — per-user theming, profile cards, DM sidebar cards, avatar theming.
- **Image crop editors for profile and server images** (internal development reference omitted).
- **Username restrictions, period support, yearly change cooldown** (internal development reference omitted).
- **OS-level permission management** (internal development reference omitted) — request, check, and enforce system permissions; closes #197.
- **Friend requests** (internal development reference omitted) — accept/decline UI with context menu.
- **Notification sounds** for chat and voice events (internal development reference omitted, internal development reference omitted); per-category sound volumes (internal development reference omitted); DM call sounds with looping (internal development reference omitted).
- **Developer Mode toggle** (internal development reference omitted) — DevTools accessible in Alpha/Beta builds only via the developer mode setting.
- **Code signing — macOS** (internal development reference omitted) — Developer ID Application cert wired to sign and notarize macOS builds.
- **Code signing — Windows** (internal development reference omitted) — Microsoft Artifact Signing for `Setup.exe`; closes #404.
- **Docker network segmentation** (internal development reference omitted) — service isolation, request-ID propagation, Redis auth bans.
- **CI/CD pipeline** (internal development reference omitted, internal development reference omitted) — GitHub Actions `build.yml` with parallel test, coverage, and SonarQube; later hardened with Semgrep SAST in internal development reference omitted.
- **Shai-Hulud 2.0 supply-chain IOC scanner** (internal development reference omitted) — closes #715; IOC list refreshed in internal development reference omitted.
- **AI governance framework** (internal development reference omitted–internal development reference omitted, internal development reference omitted–internal development reference omitted) — AI-generated code policy, CODEOWNERS, agentic controls, Semgrep SAST, path-scoped internal AI-assistant rules, Claude Code skills, custom agents, Copilot prompt templates, MCP project config.

### Changed

- **React 18 → 19.2.4** (internal development reference omitted).
- **react-router-dom 6 → 7.13.1** (internal development reference omitted).
- **Zustand 4 → 5.0.12** (internal development reference omitted).
- **ESLint → 10 flat config** (internal development reference omitted, internal development reference omitted, internal development reference omitted).
- **Vite 7 → 8.0.0** (internal development reference omitted) plus `@vitejs/plugin-react` 4 → 6.
- **Go 1.24 → 1.26.1** (internal development reference omitted) with `govulncheck` hardening.
- **Electron 33 → 41.x**, **mediasoup 3.13 → 3.19.18**, **mediasoup-client → 3.18.7**, **TypeScript 6.0.2**, **typescript-eslint 8.58**.
- **E2EE password-derived key** — PBKDF2 → Argon2id client-side (internal development reference omitted).
- **macOS notarization** switched to App Store Connect API key (internal development reference omitted).
- **Cognitive complexity reduction** across Go control-plane handlers and TypeScript frontend / media-plane (internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted–internal development reference omitted).
- **Documentation audit** completed for the v0.2.0-Beta release gate (internal development reference omitted) — drift inventory tracked internally; PR-1 verification merged in internal development reference omitted.
- **MCP server deployment refactored into per-host configs** (internal development reference omitted) — closes #778. Splits into `.mcp.json` (Claude Code CLI / App via `launchctl setenv`) and `.vscode/mcp.json` (VS Code native MCP via `${input:VAR}` → secret store). Eliminates `launchctl setenv` exposure for VS Code native MCP — OWASP A02 (Security Misconfiguration) win. Policy doc (`docs/policies/mcp-server-policy.md`) rewritten with the three-surface credential taxonomy.

### Removed

- **Sentry telemetry** stripped from all three services (internal development reference omitted control-plane, internal development reference omitted media-plane, internal development reference omitted Electron client) plus a closing sweep through MCP, CI, and rules (internal development reference omitted). The Sentry MCP server config was finally removed in the per-host MCP cleanup (internal development reference omitted, closing the MCP-config dimension). Closes #610, #614, #672. The integration that landed earlier in the cycle (#586, #622, #623, #668, #682) was reversed once telemetry surfaced zero production logs and forced re-consent friction; project memory `Sentry — being removed` documents the decision.
- **Postgres MCP server config** removed alongside Sentry MCP (internal development reference omitted) — unused operationally; final state is 6 servers in `.mcp.json` + 6 in `.vscode/mcp.json`.
- **Deprecated `WebSocketMessage` source-compat type alias** removed from `client/desktop/src/renderer/services/websocketService.ts` (internal development reference omitted) — the discriminated-union migration (internal development reference omitted / PR internal development reference omitted) made the shim unused. `WebSocketEvent` remains the canonical name.

### Fixed

- **E2EE channel-key OperationError** with structured diagnostic envelope (internal development reference omitted).
- **E2EE voice / video codec collision** — WebRTC BUNDLE misrouting between consumers (internal development reference omitted, internal development reference omitted, internal development reference omitted).
- **E2EE Insertable Streams → RTCRtpScriptTransform** migration (internal development reference omitted).
- **E2EE key request flood** — session-scoped cache plus CI hardening (internal development reference omitted).
- **E2EE frame decryption** recovery and CSK rotation hardening (internal development reference omitted, internal development reference omitted).
- **DM key-epoch enforcement, presence sync, and key distribution** (internal development reference omitted).
- **Replied-to message decryption** on REST message fetch (not only WebSocket) (internal development reference omitted).
- **DM thread real-time preview and reorder** (internal development reference omitted) — closes #486.
- **Message editing** in E2EE channels and DMs (internal development reference omitted).
- **Voice and video** — black-screen recovery, codec selection, screen-share audio, audio persistence on navigation (internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted, internal development reference omitted).
- **Hub goroutine races** with test cleanup — flaky voice tests resolved (internal development reference omitted); deterministic channel-based sync replaced `time.Sleep` in WebSocket hub tests (internal development reference omitted).
- **WebSocket reconnect** — connection-lost handler replaced page-reload with direct WS reconnect (internal development reference omitted, internal development reference omitted).
- **Self-user shown as Offline despite an active connection** — Member List, UserPopover, and profile now reconcile `selfStatus` from the connect-time presence snapshot (internal development reference omitted) — closes #803.
- **Postgres "invalid length of startup packet" flood** (internal development reference omitted) — closes #755.
- **PiP child window** loads SPA route, not marketing site (internal development reference omitted) — closes #802.
- **PiP window signaling and local user identification** (internal development reference omitted).
- **Desktop login on bundled SPA fallback** uses `app://` scheme (internal development reference omitted) — closes #830.
- **MFA verify response** parsing — `access_token` extracted from `/mfa/verify` (internal development reference omitted).
- **electron-updater trust path** hardening on macOS / Windows / CI (internal development reference omitted).
- **Build-desktop CI ASAR integrity** verification (internal development reference omitted) — closes #683.
- **Preload bundling with esbuild** for sandbox compatibility (internal development reference omitted).
- **MFA encryption key wiring** through Docker Compose (internal development reference omitted).
- **Klipy GIF media proxy 401** — webRequest auth injection (internal development reference omitted); proxy routes nested under `/gifs` (internal development reference omitted).
- **Klipy GIF rendering** — envelope unwrapped on every decrypt path; nested rendition shape parsed correctly (internal development reference omitted).
- **NATS server config and coturn TLS / external-IP** (internal development reference omitted, internal development reference omitted).
- **Modal nested Escape handler** firing on all stacked modal instances (internal development reference omitted).
- **Server role styling** isolated from DM message rendering (internal development reference omitted).
- **Accessibility pass** — semantic HTML, keyboard navigation, ARIA, UI polish (internal development reference omitted, internal development reference omitted, internal development reference omitted).
- **Theme markdown syntax help modal and help icon** (internal development reference omitted).
- **Cloudflare beacon verification** + `/spa/` nginx route + defensive sentinel (internal development reference omitted) — closes #750.

### Security

- **`Error.cause` propagation closed** (internal development reference omitted) — `console.error` / `console.warn` no longer pass raw `Error` arguments through main-process logs; ESLint enforcement and a Vitest regression test added.
- **Token-fingerprint leaks removed** (internal development reference omitted) — 10 token-suffix leaks in `tokenManager.ts` removed; ESLint warnings remediated and security rules promoted to error.
- **External-link scheme tightened** (internal development reference omitted) — `setWindowOpenHandler` and `will-navigate` restricted to `https:`-only with ESLint drift defense; user-initiated `open-external` IPC retains the broader `http:` / `https:` / `mailto:` policy. Closes #754.
- **electron-updater TLS certificate pinning** on `api.concordvoice.chat` (internal development reference omitted).
- **nginx hardening** — H2C smuggling vector closed; Host header injection blocked (internal development reference omitted).
- **GitHub Actions shell injection** resolved (Semgrep) (internal development reference omitted).
- **CORS hardening** — null/empty origin rejection, custom header validation (internal development reference omitted).
- **Hardcoded dev credentials removed**, production guards added (internal development reference omitted).
- **Scanner hardening** — brute-force probe mitigation at infrastructure level (internal development reference omitted); production infrastructure hardened against vulnerability scanners (internal development reference omitted).
- **Dependabot vulnerability fixes** — npm overrides for transitive vulnerabilities (internal development reference omitted, internal development reference omitted).

### Known Issues

Tracking at time of v0.2.0-Beta release. For the full open-issue list, see internal development reference omitted.

- **internal development reference omitted — "No Internet" Retry button stuck after network restore** — after the desktop client trips the no-internet dialog, the Retry button does not visibly progress once connectivity is back; the user must use Exit App to break out and relaunch. Workaround: quit and relaunch the desktop client once the network is restored.
- **internal development reference omitted — Markdown rendering correctness in chat** — H1 renders smaller than H2, fenced code blocks parse incorrectly in some inputs, and vertical spacing is heavier than expected. Cosmetic only; message content is preserved. Workaround: none required for delivery; fix tracked for v0.2.x.
- **internal development reference omitted — Pinned Messages panel drops media and shows GIFs as raw JSON** — pinned messages with image attachments lose the image; pinned KLIPY GIFs render as the raw envelope text. The original message in the main chat is unaffected. Workaround: scroll to the source message in the channel for the full rendering.
- **internal development reference omitted — KLIPY GIF picker hits 429 during normal scrolling** — the shared rate limiter for the GIF media proxy and the API endpoint is too aggressive when ~30 picker tiles fan out simultaneously. Workaround: pause briefly between scrolls in the GIF picker; the limiter resets quickly.
- **internal development reference omitted — Member List "+ Add Role" dropdown clipped inside the card** — the role-picker dropdown is constrained by its container and adds an inner scrollbar instead of overflowing the card. Workaround: scroll inside the Member List card to access roles below the fold.
- **internal development reference omitted — Profile editor loses in-progress edits on background user updates** — `ProfileInfoForm`'s reset effect re-runs on any user object mutation, which can wipe unsaved field edits if the underlying user object refreshes mid-edit. Workaround: save profile changes promptly; avoid leaving the editor open while presence or other user-object events fire.

### Migration from v0.1.0-Alpha

**No end-user breaking changes.** New features (Apple/Google SSO, MFA/WebAuthn, channel-key revocation, server mute/deafen, Klipy GIF integration, DM message pinning, account erasure, bundled-SPA fallback) are additive — existing v0.1.0-Alpha installations continue to work without manual intervention.

**Automatic migrations applied on first connection / first server run:**

- Database schema additions (migrations 000054–000061): server mute/deafen state, Klipy GIF customer IDs, DM message pinning index, pending registrations TTL, account-deletion cascade, removed `sentry_delete_attempted` column, and SSO identities (`is_relay_email` for Apple privacy-relay handling).
- Refresh-token cascade (migration 000059): `refresh_tokens.user_id` becomes `ON DELETE CASCADE`, which strengthens the atomic-revocation invariant for account erasure.
- E2EE key derivation: PBKDF2 → Argon2id (internal development reference omitted) migrates transparently on next login — legacy keys are unwrapped with PBKDF2, re-wrapped with Argon2id, and uploaded to the server. No user action required; migration 000034 adds `key_derivation_alg` tracking.
- Renderer migrations: existing local data migrates client-side via the standard Zustand `persist` migration path on first launch; no user-visible state loss.

**Behavior changes that may surprise:**

- **External link policy** (internal development reference omitted, internal development reference omitted): the Electron client's `setWindowOpenHandler` and `will-navigate` now restrict to `https:` only — passive navigation (redirects, programmatic `window.open`) cannot escape to the OS browser for non-`https:` schemes. User-clicked links in `UserProfileModal` and the Markdown pipeline (`SafeLink.tsx`) route through the `open-external` IPC handler, which accepts `{http, https, mailto}` because the explicit click is consent. Legacy `http://` profile links continue to work via this path.
- **Token revocation on password change**: refresh tokens are now atomically revoked when a user changes their password (was a known gap in Alpha). Active sessions on other devices will be logged out on the next refresh attempt. This is the intended behavior; surfaced here because v0.1.0-Alpha did not enforce it.
- **Sentry telemetry removed**: zero behavior change to end users (telemetry was opt-in and zero events were captured in production); flagged for transparency.

**For self-hosted operators:**

- Review the migration set `services/control-plane/migrations/` (000054 onward represents the Alpha→Beta delta) for any deployment-specific actions.
- The MCP server cleanup (internal development reference omitted) is dev-environment-only — affects contributor tooling, not production deployment. No action required for self-hosted operators.
- For Apple sign-in support, configure the Apple Sign In credentials per Apple Developer documentation: Team ID, Key ID (with the corresponding `.p8` private key), Services ID, and the loopback redirect URI. Set the corresponding env vars in your control-plane `.env` (see `services/control-plane/internal/oauth/` for the variable names; the source-of-truth is `apple_clientsecret.go`). Without these, the Apple SSO button will be visible but sign-in attempts will fail.
- macOS code signing now uses App Store Connect API key (internal development reference omitted) — only relevant if you build your own signed binaries; the public release builds remain Apple Developer ID signed.

---

## [0.1.41] — 2026-05-20 (#806 cross-platform window chrome)

### Added

- **Cross-platform native window controls** — Windows + Linux now show native close / minimize / maximize buttons in the top-right via Electron's `titleBarOverlay` API; macOS retains its native traffic lights via `titleBarStyle: 'hiddenInset'`. The per-platform branching lives in `client/desktop/src/main/browserWindowConfig.ts` (internal development reference omitted).
- **Branded Titlebar** — new `<Titlebar />` component renders centered `CONCORD VOICE` in BaronNeue.woff with the running version + active SPA hash (`v0.1.41-abc123`). The version line updates live when an SPA hot-update lands via the new `spa:versionChanged` IPC event.
- **Window state persistence** — size + position + maximized state save to `window-state.json` under `app.getPath('userData')` with 500ms debounce on resize/move and synchronous write on close. Restore validates the saved bounds against the current display layout (4 safety checks: NaN/missing, display intersection, min/max size, negative-coords). Wayland sessions omit x/y per compositor-controlled placement.
- **Client Behavior settings** — new Settings → Appearance subsection lets users assign the `[×]` close and `[—]` minimize buttons to system tray, OS taskbar/dock, or graceful quit. Mutex + coverage rules visualize invalid configs as greyed-out segmented-control cards with explanatory `title=""` tooltips. Dynamic explanation panel reads the current configuration and renders 3 "How do I X" paragraphs.
- **Coordinated-pair sibling internal development reference omitted** — system tray icon. v0.1.41 ships the Client Behavior surface; #1099 ships the tray icon. Both required for the `[X] → tray` and `[-] → tray` paths to be user-visible.

### Changed

- **IPC surface widened** — 4 new `window:*` channels (`setClientBehavior`, `quit`, `setTitleBarOverlayColor`, `getVersionString`) + 1 new send-only event (`spa:versionChanged`). All 4 handlers carry runtime input validators at the IPC trust boundary; sender-frame validation is intentionally omitted per the new "Low-stakes UI-state IPC" exception class codified in the internal Electron security rules.
- **PiP windows opt out of OS-drawn shadow** — `hasShadow: false` on PiP `BrowserWindow` construction for the lightweight floating-glass aesthetic. macOS drops the standard window shadow; no-op on Wayland/X11/Windows.
- **Theme-color sync** — settingsStore subscribes to `appearance.theme` and IPC-pushes resolved overlay colors via `window:setTitleBarOverlayColor` on every theme change, including the OS-driven `prefers-color-scheme` listener for `theme: 'system'`. macOS ignores the IPC (uses native traffic lights); other platforms get the dark / light overlay treatment.

### Documentation

- **Developer handoff spec** for the Client Behavior section landed in the internal docs tree.
- **Internal Electron security rules** now document the low-stakes-IPC sender-frame exception class with conditions, current accepted-exception handlers, and an explicit list of categories that MUST validate.

### Deferred / follow-up

- Plan Task 20 (cross-platform manual verification on Windows 11 + Ubuntu GNOME) requires real hardware. macOS verification will land before merge.

---

## [0.1.40] — 2026-05-20 (Linux build hotfix)

> v0.1.39 was bumped in `package.json` on `main` but never received a GitHub
> Release: the `build-desktop.yml` build matrix had Linux build failures (see
> "Fixed" below), so the `release:` job correctly skipped per ADR-0004
> Invariant 1 (`needs.build.result == 'success'` gate). v0.1.40 carries the
> full v0.1.39 release content plus the Linux build fix.

### Fixed

- **Linux build no longer fails at `@reforged/maker-appimage` packaging** — commit internal development reference omitted ("Change company name to 'Concord Voice LLC'") silently bundled an unrelated `packagerConfig.executableName` rename from `'concord-voice'` to `'Concord Voice'` alongside its legitimate company-name updates. With executableName changed to `'Concord Voice'`, the packaged Linux binary became `Concord-Voice-linux-<arch>/Concord Voice` (with a space), but the Linux makers' `bin: 'concord-voice'` option still performed a literal-string file lookup for `concord-voice`, failing with `"Could not find executable 'concord-voice' in packaged application"` (internal development reference omitted). The bug sat latent on `main` for ~6 hours while the cascade-skip regression (since-fixed by internal development reference omitted) was still suppressing the build matrix, and surfaced on the first `push:main` after the cascade-skip fix shipped — exactly the verification path documented in ADR-0004 (desktop release contract) Invariant 3. Fixed by making `executableName` per-platform: Linux falls back to kebab-case `'concord-voice'` (matching the maker `bin:` lookup and debian-policy §5.6.7), while macOS / Windows retain the proper-name format `'Concord Voice'` (visible in Activity Monitor, Task Manager, and crash reports).

### Changed

- **Test guard for the Linux ↔ display-name asymmetry is now platform-conditional** — `packagingIdentity.test.ts` updates the `executableName` literal-value assertion and the `Linux maker bin intentionally diverges from executableName` asymmetry-guard to branch on `process.platform`. On Linux test runners (CI's ubuntu-latest), the asymmetry guard returns early because the per-platform conditional in `forge.config.ts` makes Linux's `executableName` legitimately equal to `bin`; on macOS/Windows, the guard still asserts the deliberate divergence.

---

## [0.1.39] — 2026-05-20 (Release-pipeline fix + v0.1.36–v0.1.38 catch-up)

> First desktop release published via the workflow since v0.1.34 (2026-05-02).
> v0.1.35 was published manually by the operator on 2026-05-09; v0.1.36, v0.1.37,
> and v0.1.38 were bumped in `package.json` on `main` but never received GitHub
> Releases due to a workflow regression — see "Fixed" below. `gh release list`
> confirms no tags exist for those three versions. v0.1.39 bundles the accumulated
> v0.1.36–v0.1.38 content with the workflow fix that ships it.

### Fixed

- **Desktop release workflow no longer cascade-skips on `push:main`** — PR #889 (merged 2026-05-08) introduced a `pr-paths-filter` job to support PR smoke-testing. That job carries `if: github.event_name == 'pull_request'` and is `skipped` on push events. The downstream `release` job did not opt out of GHA's transitive-skip semantics with `always()`, causing every release-bearing push to main since 2026-05-08 to skip the `Create release` step despite all six platform builds succeeding. Fixed by adding `always()` with explicit `.result == 'success'` checks on the direct upstream needs, plus the original `should_release == 'true'` gate.

### Changed

- **Product display name normalized to "Concord Voice"** (was "ConcordVoice"; commit 72b98f1a) — affects the macOS Dock label, Windows registry display name, and Linux desktop-entry name in the packaged builds.
- **Company name normalized to "Concord Voice LLC"** (commit 21accce5) — License + About-screen attribution; aligns with the Windows Authenticode signer CN already pinned in `eslint.config.mjs` and the Windows signature verification step.
- **Per-target notification mute preferences** (internal development reference omitted) — closes internal development reference omitted; independent mute toggles for messages vs voice on a per-server/per-DM basis.

### Changed (carried over from unshipped v0.1.36–v0.1.38)

- **Backend stops emitting `is_encrypted` field across API and WebSocket envelopes** (internal development reference omitted) — the field is now structural (every room/channel is encrypted under internal development reference omitted). Inbound WebSocket envelopes lacking `key_version >= 1` are rejected via close frame 4400 `missing_or_invalid_key_version`. Landed on main in the v0.1.37 bump; first released here.
- **Frontend stops reading `is_encrypted`** (internal development reference omitted) — Child A of the #201 epic; landed on main in the v0.1.36 bump; first released here.
- **Media-plane removes `is_encrypted` field + documents SRTP-mandatory** (internal development reference omitted) — Child D of #201; landed on main in the v0.1.37 bump; first released here.

### Fixed (carried over from unshipped v0.1.36–v0.1.38)

- **WebSocket first-attempt drop is now silent + WS auth ticket redacted from console** (internal development reference omitted) — noisy reconnect log on the very first connection has been quieted; auth ticket no longer surfaces in DevTools console. Landed on main in the v0.1.38 bump; first released here.

---

## [0.1.18] - 2026-04-09 (Phase 2B — Sentry Error Tracking)

### Added

- **Sentry Electron integration** — Error tracking for main and renderer processes with fail-closed privacy model; `beforeSend` scrubber drops key material, PII, and console breadcrumbs before transmission (#586)
- **Docs-reviewer subagent** — Claude Code subagent for automated documentation drift detection with tier classification and `/review-pr` skill dispatch

### Changed

- **Documentation refresh** — AGENTS.md, REVIEW.md, README.md, and docs/architecture.md updated to reflect current Phase 2B state

---

## [0.1.17] - 2026-04-09 (Phase 2B — QA Pass & Infrastructure Hardening)

### Added

- **Project-stats counts automation** — a maintenance script that keeps internal project statistics in sync (#581)

### Changed

- **MCP env var standardization** — Environment variable naming aligned across MCP server configs (#581)

### Fixed

- **KLIPY GIF proxy routes** — Nested `/gifs` path prefix added to match client-side route expectations (#580)
- **Copilot review feedback** — Addressed review comments from PR #577 (#579)
- **coturn TLS hardening** — Certificate isolation, `turn.concordvoice.chat` SAN added, certbot deploy hook wired
- **NATS single-node config** — Corrected NATS configuration; coturn TLS and external-IP wiring fixed

---

## [0.1.16] - 2026-04-08 (Phase 2B — Media Proxy & QA)

### Added

- **Public Tier 1 media proxy** — Unauthenticated access for public media assets (#570)

### Fixed

- **Avatar slot pinning** — Avatar elements pinned to 40×40 px to prevent layout shift (#570)
- **QA bug pass** — Broad regression sweep covering UI, API, and infrastructure issues (#571)

---

## [0.1.15] - 2026-04-07 (Phase 2B — Klipy GIF Integration)

### Added

- **Klipy GIF integration** — GIF search, picker, and privacy proxy through the control-plane; API key injected via environment variable; disclaimers and branding in About section (#483, #557)
- **Developer Mode toggle** — DevTools accessible in Alpha builds only via developer mode setting (#567)

### Changed

- **Dependency bumps** — `dotenv`, `@vitest/coverage-istanbul`, `go-webauthn/webauthn`, `@types/node`, build-tooling group (4 packages), testing group (2 packages) (#559, #560, #561, #564, #565)

### Fixed

- **Windows desktop build** — `matrix.arch` substituted directly in `electron-forge make` command to fix cross-platform CI build (#569)
- **Chat and GIF rendering** — Chat message rendering, GIF display via Klipy, MinIO crop upload, and SPA hot-reload all corrected (#566)
- **Theme-aware Klipy logos** — Logos respond to active color scheme; public/branding layout restructured
- **GIF envelope decryption** — Envelope unwrapped on every decrypt path, not only realtime; nested `file.{hd,md,sm}.<format>` rendition shape parsed correctly
- **SPA CSP headers** — Strict default CSP no longer dropped on every response, only on success paths; SPA bundle directory mounted into control-plane container
- **GIF picker UX** — Picker interaction, settings accessibility regressions, and test assertions updated

---

## [0.1.14] - 2026-04-06 (Phase 2B — Voice Refactors & RBAC Wiring)

### Changed

- **VoiceAudioSection split** — Component decomposed into focused sub-components for maintainability (#553)
- **voiceService complexity reduction** — Remaining high-complexity functions in `voiceService` refactored (#552)
- **E2EE transforms extraction** — E2EE transform and produce helpers extracted to reduce cognitive complexity (#551)
- **Codec cascade extraction** — Codec cascade selection logic extracted into a dedicated helper (#550)

### Fixed

- **RBAC context menu wiring** — Context menu actions wired to RBAC roles; moderation actions (kick, ban, mute) enabled (#548)

---

## [0.1.13] - 2026-04-04 (Phase 2B — Chat Features, Server Mute/Deafen, Infrastructure)

### Added

- **Server mute/deafen with SFU enforcement** — Server-side mute and deafen state propagated to mediasoup SFU; consumers paused/resumed on enforcement (#488)
- **Shared `useChatController` hook** — Unified chat container logic (fetch, paginate, decrypt, send) extracted into a reusable hook (#545)
- **Message reactions** — Emoji reaction add/remove on messages with real-time sync (#169, #459)
- **Reply/quote messages** — Threaded reply rendering with quoted message preview (#170, #463)
- **Message pinning** — Pin/unpin messages in channels with pin feed (#171, #465)
- **E2EE-native message search** — Client-side decrypted search across channel message history (#172, #468)
- **File and image attachments** — Upload, preview, and download for chat attachments (#178, #470)
- **Group DM creation and management** — Multi-participant DM groups with admin controls (#208)
- **Desktop notifications** — System notifications for @mentions and new DMs (#175, #478)
- **Draft message persistence** — Unsent drafts saved per-channel and restored on revisit (#174, #477)
- **Keyboard shortcuts system** — Configurable keyboard shortcuts with help overlay (#176, #479)
- **Global context menu** — Unified context menu system with clipboard support and role assignment (#446)
- **SPA deployment pipeline** — File server, versioning, and GitHub Actions workflow for hot-update SPA bundles (#429)
- **Docker network segmentation** — Service isolation, request ID propagation, Redis auth bans (#442)
- **AI governance framework** — AI-generated code policy, CODEOWNERS, agentic controls, Semgrep SAST, path-scoped internal AI-assistant rules, Claude Code skills, custom agents, Copilot prompt templates, MCP project config (#454–#458, #500–#504, #507–#518, #522)

### Changed

- **Message component decomposition** — `Message` component decomposed; shared types extracted for the Phase 2B chat rewrite (#451)
- **DM thread list real-time updates** — `last_message` included in `dm_unread_notify` events for live thread list refresh (#541)
- **Dependency bumps** — TypeScript 6.0.2, typescript-eslint 8.58, eslint 10.1, mediasoup, lucide-react 1.7.0, Electron, `@types/node`, `@playwright/test`, Go module group, Actions group, media-plane dev-tooling (#411, #414, #511, #528–#529, #532–#536, #538–#539)
- **Email infrastructure** — Transactional email migrated from Proton SMTP to Resend; branded templates; `verify.concordvoice.chat` subdomain
- **CI pipeline hardening** — Semgrep SAST added; quality gates formalized; CI performance optimized with `sync.Once` migrations, test sharding, and caching (#457, #471)
- **Go and TypeScript complexity reduction** — Cognitive complexity reduced across control-plane handlers and frontend/media-plane code (#418, #419, #498)
- **DRY modal components** — Shared modal panels extracted to eliminate duplication (#365, #481)

### Fixed

- **DM thread real-time updates** — Last message propagated in unread notify payload to refresh thread list (#541)
- **Replied-to message decryption** — `replied_to` content decrypted on REST message fetch, not only via WebSocket (#542)
- **Server role styling isolation** — Server role badge styles no longer bleed into DM message rendering (#543)
- **Hub test determinism** — `time.Sleep` replaced with deterministic channel-based sync in WebSocket hub tests (#544)
- **Context menu role assignment** — Role assignment via context menu no longer fails silently (#447)
- **Voice flaky tests** — Hub goroutine races with test cleanup resolved (#476)
- **Modal Escape handler** — Nested Escape key handler no longer fires on all stacked modal instances (#480)
- **Video frame scaling** — Video frames dynamically scale to fill voice chat area (#443)
- **WebSocket reconnect** — Connection-lost handler replaced page reload with direct WS reconnect (#194, #439)
- **Accessibility pass** — Semantic HTML, keyboard navigation, ARIA attributes, and UI polish across the app (#380, #427)
- **nginx security** — H2C smuggling and Host header injection mitigated; shell injection in GitHub Actions CI fixed (#523, #525)
- **Semgrep findings** — Verified-safe `sql-sprintf` findings suppressed with inline annotations (#524)
- **Media-plane Redis URL** — Media-plane added to data network with correct Redis URL configured

### Security

- **Nginx hardening** — H2C smuggling vector closed; Host header injection blocked (#525)
- **GitHub Actions shell injection** — Semgrep-identified injection in CI workflow resolved (#523)

---

## [0.1.12] - 2026-03-27 (Phase 2A — Foundations & Security)

### Added

- **MFA/WebAuthn authentication** — TOTP, WebAuthn/FIDO2, backup codes, recovery circles, trusted devices (#89)
- **RBAC/SBAC permission system** — Granular role-based access control with audit logging (#82)
- **Email verification** — SMTP-based verification on registration (#269)
- **Object storage (MinIO)** — User image assets migrated from PostgreSQL to S3-compatible storage (#166)
- **Server ownership transfer** — Full lifecycle with MFA, email confirmation, reversal tokens (#244)
- **Desktop auto-updater** — Safe updates with rollback, splash screen, progress tracking (#155, #381-#387)
- **Token theft detection** — Machine ID + IP binding with automatic revocation (#89)
- **Proactive token refresh** — JWT refresh before expiry with rate limiting (#240, #254)
- **CSK rotation on member removal** — E2EE forward secrecy for channel keys (#96)
- **DM key-epoch enforcement** — Key revocation table + epoch checking in WebSocket path (#122)
- **@mention routing in E2EE** — Server-side mention detection without decryption (#118)
- **OS permission management** — System-level permission requests and enforcement (#197)
- **Self-hosted coturn STUN/TURN** — Infrastructure for NAT traversal (#124)
- **CI/CD pipeline** — GitHub Actions build.yml with parallel test + coverage + SonarQube (#128, #130)
- **Test coverage push** — 70 Go test files, 195 frontend test files toward 80% Quality Gate
- **Custom branded splash screen** — Install/update progress with Concord Voice branding (#387)
- **Install/update logging** — Structured file-based logging for troubleshooting (#383)

### Changed

- **React 18 → 19.2.4** (#181)
- **react-router-dom 6 → 7.13.1** (#182)
- **Zustand 4 → 5.0.12** (#183)
- **ESLint → 10 flat config** (client + media-plane) (#184, #185, #186)
- **Redis client 4 → 5.0.0** (media-plane) (#187)
- **Vite 7 → 8.0.0** + @vitejs/plugin-react 4 → 6 (#287)
- **Go 1.24 → 1.26.1** + govulncheck hardening (#193)
- **Electron 33 → 41.0.2** (desktop client)
- **mediasoup 3.13 → 3.19.18** (server), mediasoup-client → 3.18.7
- **E2EE key derivation** — PBKDF2 → Argon2id client-side (#117)
- **useMessageFetch hook** — Extracted shared fetch/decrypt/paginate logic (#177)
- **Desktop bundle naming** — Unified to "Concord Voice" across all platforms (#385)
- **OS-level app metadata** — Correct version, icon, publisher on all platforms (#386)
- **Shared updater resources eliminated** — Prevents conflicts with other Electron apps (#382)

### Security

- **CORS hardening** — Null/empty origin rejection, custom header validation (#259)
- **Credential extraction** — Hardcoded dev credentials removed, production guards added (#260)
- **Scanner hardening** — Brute-force probe mitigation at infrastructure level (#153)
- **Dependabot vulnerability fixes** — npm overrides for transitive vulnerabilities (#289)

### Fixed

- **E2EE voice/video codec collision** — WebRTC BUNDLE misrouting between consumers (#291)
- **Mac microphone in E2EE voice** — Audio production fixed in encrypted channels (#295)
- **Video streaming quality** — Codec selection and screen share audio (#299)
- **Voice audio persistence** — Audio no longer cuts out when navigating away (#396)
- **Voice/video black screens** — Audio output and video rendering restored (#227)
- **Message editing** — Edit submit no longer silently dropped (#161)
- **Identity asset theming** — Uses viewed user's color scheme, not viewer's (#165)
- **Popover toggle** — Clicking again dismisses instead of respawning (#167)
- **Screen share defaults** — Respects Video Configuration settings (#198)
- **UpdateRole response** — Handler now returns role body, preventing crash (#249)

## [0.1.0-alpha] - 2026-03-03 (Phase 1 — Core Platform)

### Added

- **Phase 1A: Authentication & E2EE** — User registration, JWT + refresh tokens, E2EE (RSA-OAEP 4096 + AES-256-GCM), session management, Argon2id password hashing, rate limiting
- **Phase 1B: Channels & Text Chat** — Server/channel CRUD, WebSocket messaging, E2EE encryption/decryption, presence system, 12 color schemes (dark/light), security hardening, API docs (OpenAPI 3.0)
- **Phase 1C: Voice, Media & Desktop** — mediasoup SFU (voice, video, screen share), 7 audio quality tiers, video codec selection (VP9/AV1/VP8/H.264), channel groups/categories, Electron safeStorage, emoji picker, DM framework, custom theme builder, mic test with loopback
- **Infrastructure** — Docker Compose (dev/staging/production), coturn STUN, cross-platform Electron build, pre-commit hooks, Dependabot
