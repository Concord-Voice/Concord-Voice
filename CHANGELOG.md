# Changelog

All notable changes to Concord Voice will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [0.2.25] — 2026-07-09

### Security

- **Archive handling hardened against malicious files** ([#2142](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2142)) — updated the tar library used by our desktop build tooling and the voice server's installer so maliciously crafted archives (compression bombs and malformed headers) can no longer hang or crash those steps. Routine hygiene — no user action needed.

## [0.2.24] — 2026-07-05

### Fixed

- **A clearer signal when your encryption keys can't be saved** ([#2068](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2068)) — if your device keychain is locked or full when Concord saves your encryption keys for next launch, the app now notices instead of failing silently. Your current session keeps working either way; if saving didn't succeed, signing in again on the next launch restores it.

## [0.2.23] — 2026-07-04

### Added

- **Manage your subscription in the app** ([#2043](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2043)) — a new Settings ▸ Subscription page lets you redeem a code and see your current plan and status at a glance.
- **Audio and video quality that follows your plan** ([#2042](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2042), [#2039](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2039), [#2038](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2038)) — call quality, per-room camera and screenshare limits, and server capabilities now track your tier. When bandwidth gets tight, the app sheds webcam before screenshare before audio, so voice stays clear.
- **Higher limits for subscribers** ([#2040](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2040)) — how many servers you can create and how deep you can search now scale with your subscription.
- **Smoother GIFs, safely** ([#2041](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2041)) — the GIF pipeline preserves animation while guarding against maliciously oversized files.

### Fixed

- **Single sign-on no longer times out mid-signup** ([#2047](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2047)) — SSO registration now gets a proper 15-minute token window and recovers gracefully if it expires, instead of stranding you partway through.

### Changed

- **Behind-the-scenes CI, supply-chain hardening, and dependency upkeep** ([#2049](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2049), [#2046](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2046), [#2036](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2036), [#2035](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2035)) — tighter least-privilege on the release workflows, a security-patched build toolchain, and routine dependency refreshes.

## [0.2.22] — 2026-07-02

### Added

- **See what changed, right after you update** ([#2034](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2034)) — update Concord Voice, and the next launch shows a "What's new" dialog covering every version since the one you had. It shows once, works offline, and never slows startup. Read it or dismiss it — your call.
- **A changelog you can hold us to** ([#2034](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2034)) — this file is now the canonical public record of every change we ship. Every release since Beta is documented below, and CI refuses to cut a new version without its entry. No entry, no release.

## [0.2.21] — 2026-07-02

### Added

- **Bigger image uploads for subscribers** ([#2010](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2010)) — avatar, banner, and server-icon size limits now scale with your subscription tier.

### Changed

- **Behind-the-scenes tooling, CI, and documentation upkeep** ([#2016](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2016), [#2017](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2017), [#2019](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2019), [#2025](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2025)).

### Fixed

- **Server message pinning works again** ([#2011](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2011)) — and pinned media now stays inside its panel.
- **No more scroll jumps** ([#2008](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2008)) — switching channels quickly no longer snaps you back to a stale position.
- **Previews that speak plainly** ([#2015](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2015)) — DM and notification previews now say "Photo" instead of a raw placeholder.
- **The composer menu renders where it belongs** ([#2014](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2014)) — above the placeholder text, not behind it.
- **The client asks for less** ([#1990](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1990), [#2009](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2009)) — removed permission checks the client was never entitled to make. Fewer requests to the server, less noise on the wire.
- **One path for update checks** ([#2013](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2013)) — the app's UI and binary update checks now share a single, consistent path.
- **Cleaner sign-up hint** ([#2018](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2018)) — the suggested identity no longer carries a stray "@concordvoice.chat" suffix.

### Security

- **Windows verifies every update before it installs** ([#2023](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2023)) — the update manifest now names the expected publisher, so Windows checks the code signature of each downloaded update at install time.
- **Proof, not promises, for public releases** ([#1988](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1988)) — every publicly mirrored desktop release asset now carries verifiable build provenance. You don't have to take our word for what's in the installer — verify it.
- **Attestation misconfiguration now fails loud** ([#2024](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2024)) — corrected the verification default and added a guard so a misconfigured deployment cannot pass silently.

## [0.2.20] — 2026-06-30

### Added

- **Self-host it, get everything** ([#1985](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1985)) — run Concord Voice on your own hardware and every premium entitlement is unlocked out of the box. No subscription required. That is the self-hosting deal: your server, your rules, all of it.
- **Legal documents, one click away** ([#1987](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1987)) — the license, terms, and other notices are readable directly from Settings ▸ About.

## [0.2.19] — 2026-06-30

### Added

- **Sign in to your own server** ([#1982](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1982)) — the first slice of self-hosted support: desktop login can now route to an operator-run Concord Voice server instead of the managed service.

### Fixed

- **Stranded clients can find their way home** ([#1984](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1984)) — the app now checks the public release feed for updates, so a client stuck on an old version can always recover and get current again.

### Changed

- **Behind-the-scenes deployment and tooling upkeep.**

## [0.2.18] — 2026-06-29

### Security

- **Certificate pinning restored** ([#1983](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1983)) — our certificate pinning for Cloudflare-fronted connections had lapsed. This release brings it back: connections to Concord Voice services are once again verified against the exact certificates we expect. We are telling you it lapsed because you deserve to know — that is the point of this file.

## [0.2.17] — 2026-06-29

### Added

- **React to direct messages** ([#1976](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1976)) — DMs now take emoji reactions, the same way server channels already do.

### Changed

- **Behind-the-scenes tooling, CI, and dependency upkeep** ([#1979](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1979)).

### Fixed

- **You connect reliably, right from launch** ([#1978](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1978)) — we removed a race in the WebSocket authentication handshake that could leave the client sitting disconnected just after startup.
- **Certificate rotation no longer locks you out** ([#1980](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1980)) — clients enforcing the old production API certificate pin failed to connect after we rotated the certificate. The client now trusts the rotated pin.

## [0.2.16] — 2026-06-29

### Added

- **Choose what your notifications reveal** ([#1970](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1970)) — a new setting controls how much message content appears in desktop notifications, so nothing private shows up on a shared or public screen unless you want it to.
- **Custom themes are now free** ([#1974](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1974)) — build your own theme, no subscription required.

### Fixed

- **Your call keeps playing when you open DMs** ([#1972](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1972)) — the voice audio pipeline stays mounted while you browse direct messages, so call audio no longer drops mid-navigation.
- **macOS relaunches after installing an update** ([#1973](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1973)) — "Restart to update" on macOS quit the app and then didn't bring it back. The update-install restart path now reopens Concord Voice for you.

### Changed

- **Behind-the-scenes tooling, CI, and supply-chain upkeep** ([#1968](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1968), [#1969](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1969)) — we retired a legacy automation credential and automated the guard on our supply-chain indicator-list refresh. Nothing changes for you; the pipeline gets safer.

## [0.2.15] — 2026-06-29

### Added

- **Member timeout moderation** ([#1967](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1967)) — moderators can put a member in timeout for a set duration. Participation is restricted until the clock runs out — no permanent ban required for a temporary problem.
- **macOS Applications-folder move prompt** ([#1966](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1966)) — the first time you launch Concord Voice from outside /Applications, it offers to move itself there. One click gets you a proper install and updates that land reliably.

### Changed

- **Behind-the-scenes tooling, CI, and deployment upkeep.**

### Security

- **Stricter passkey registration requirements** ([#1963](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1963)) — WebAuthn registration now demands passkey-grade platform authenticator options. Every new passkey you create meets the security bar we set — no silent downgrades.
- **Access token revoked on account erasure** ([#1965](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1965)) — erase your account and your current access token dies with it. No authenticated session outlives the deletion.

## [0.2.14] — 2026-06-28

### Added

- **Per-channel audio quality standard** ([#1946](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1946)) — set one audio quality tier on a voice channel and every member gets it, bounded by the server's tier. A new slider in channel settings puts the choice where you'd expect it.
- **SVC / Simulcast casting toggles** ([#1929](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1929)) — advanced video controls in Settings ▸ Audio & Video let you decide which layered-video codec modes your client publishes with.
- **Self-hosted TLS certificate provisioning** ([#1919](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1919)) — self-hosting? One script provisions your origin certificate: self-signed, Let's Encrypt, or bring your own.

### Changed

- **My Profile moved into Settings ▸ Account** ([#1930](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1930)) — profile editing now lives in Settings, where the rest of your account already is; the old quick-link deep-links to the same place. The SSO controls on the Security page got a polish pass too ([#1951](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1951)).
- **Behind-the-scenes tooling, CI, and dependency upkeep** — supply-chain threat-list refreshes, deploy-summary and mirror-sync fixes, and dev-environment maintenance.

### Fixed

- **Username case-handling consistency** ([#1936](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1936)) — usernames are now case-insensitive everywhere. This closes three real bugs: an SSO mixed-case lockout that blocked profile edits, duplicate accounts differing only by case, and friend-add lookups that failed on capitalization.
- **macOS notification permission status** ([#1960](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1960)) — Concord Voice now reconciles its notification settings with the actual macOS permission state, so what you see in Settings matches what your Mac will do.
- **Self-hosted TLS failures no longer silent** ([#1964](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1964)) — local certificate provisioning errors were being swallowed. Now they're reported to you, the operator, plainly — a failure you can see is a failure you can fix.

### Security

- **Hardened email MFA setup for production** ([#1961](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1961)) — tightened the email-based multi-factor enrollment flow for production.
- **Cached-UI origin allowlisted ahead of activation** ([#1928](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1928)) — the signed offline UI cache's origin is now on the server allowlists before the cache goes live, so passkey login and voice keep working the moment that fallback activates.

## [0.2.13] — 2026-06-28

### Fixed

- **Invite bubbles now name the person who actually invited you** ([#1909](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1909)) — Send-to-a-Friend invite messages in chat were attributed to the wrong user. Now the sender shown is the sender who sent it.
- **System Permissions settings are easier to find and understand** ([#1910](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1910)) — clearer wording and better navigation for the System Permissions section in Settings, so you know exactly what the app can touch.

### Security

- **Linux updates are signature-verified before they install** ([#1923](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1923)) — AppImage, deb, and rpm update artifacts now carry detached Ed25519 signatures, verified against a public key bundled in the client. An update that fails verification does not install. No exceptions.
- **Signed offline UI cache is live** ([#1907](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1907), [#1908](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1908)) — when the latest UI can't be fetched, the desktop client falls back to a last-known-good copy that is cryptographically verified against an embedded public key before it runs. You get a working app; you never get unverified code.

### Changed

- **Behind-the-scenes tooling, CI, and configuration upkeep** ([#1918](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1918), [#1920](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1920)) — removed a retired sign-in configuration value from internal checks and documentation.

## [0.2.12] — 2026-06-27

### Fixed

- **Your camera and screen share stay live when video quality layers switch** ([#1903](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1903)) — re-negotiating video layers mid-call could stop the underlying capture track and take down every camera in the room. Producers now keep the track alive across the switch.
- **Age-verification status survives a reload** ([#1904](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1904)) — verified state is re-fetched from the server on startup, so if you've already verified, you won't be asked again.

### Changed

- **Behind-the-scenes tooling, configuration, and code-quality upkeep** ([#1900](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1900), [#1901](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1901), [#1906](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1906)) — engineering write-ups, groundwork for a storage-configuration rename, and static-analysis cleanup. Nothing changes in how the app behaves for you.

## [0.2.11] — 2026-06-26

### Fixed

- **Hardware video encoding now matches what your GPU can actually do** ([#1879](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1879)) — Concord Voice now queries your system's supported hardware encode profiles instead of assuming a fixed codec set, so camera and screen-share encoding picks codecs your GPU genuinely accelerates.

## [0.2.10] — 2026-06-26

### Added

- **Run your own Concord Voice instance** ([#1892](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1892)) — a guided installer walks you through standing up a self-hosted instance, and a new `concord-selfhost` command starts, stops, monitors, and health-checks the stack.

### Fixed

- **AV1 video no longer black-screens in end-to-end-encrypted calls** ([#1896](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1896)) — AV1 camera and screen-share video could fail to decrypt, leaving you staring at a black screen. We reworked per-frame media encryption (frame crypto v4) so AV1 streams decrypt reliably — encryption stays on, and your video stays visible.

### Changed

- **Behind-the-scenes tooling, CI, and dependency upkeep** ([#1877](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1877), [#1894](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1894)) — refreshed our supply-chain threat indicators and taught macOS release builds to retry a transient DMG packaging flake.

## [0.2.9] — 2026-06-24

### Added

- **Signed offline UI cache** ([#1880](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1880)) — the desktop app keeps a cryptographically signed last-known-good copy of the latest UI. If the update server is briefly unreachable, you still start with the current interface — verified before it loads, not just cached.
- **Server capabilities discovery endpoint** ([#1883](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1883)) — a new `GET /api/v1/server/capabilities` endpoint tells your client what a server — including a self-hosted one — supports before you connect.

### Fixed

- **Video no longer black-screens after a mid-call key rotation** ([#1885](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1885)) — encrypted video frames now carry their channel-key version, so receivers select the correct decryption key after an end-to-end-encryption key rotation. Your video stays up; the encryption stays on.
- **Public release mirroring now triggers only on real releases** ([#1887](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1887)) — we corrected the condition that publishes signed builds to the public downloads page. It fired when it shouldn't have; now it fires exactly when a release ships.

### Security

- **Stricter validation of the API server address in packaged builds** ([#1882](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1882)) — the app now accepts only the official service address or a self-hosted server that passed the app's own verification probe. Hardens the client against a compromised UI steering it somewhere else.

### Changed

- **Behind-the-scenes tooling and engineering-workflow upkeep** ([#1884](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1884), [#1888](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1888)).

## [0.2.8] — 2026-06-24

### Changed

- **A cleaner invite landing page** ([#1869](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1869)) — open a server invite link and you land on a restyled page that gets you into the server without ceremony.
- **Behind-the-scenes tooling, CI, and test upkeep** ([#1864](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1864), [#1871](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1871), [#1875](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1875)) — including expanded automated color-contrast (WCAG) checks, so accessibility regressions get caught by machines, not by you.

### Fixed

- **UI updates reach you reliably, and sessions survive a soft reload** ([#1872](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1872)) — the client retries fetching the latest UI after a cold-start network hiccup and checks for newer UI on its own (waiting until you're out of a call). Session-only sign-in — "Remember Me" off — now keeps your credentials and encryption keys across an in-app reload, held in memory only. They never touch disk.
- **Encrypted video stays in sync with layered and simulcast streams** ([#1865](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1865)) — we hardened the end-to-end-encryption key epochs for media, so camera and screen-share video no longer desyncs or black-screens when the stream is layered.
- **Switch audio devices mid-call** ([#1866](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1866)) — change your microphone or speaker during a call and the active audio streams retarget correctly. No rejoin required.
- **DM voice calls open the call view and play remote audio** ([#1876](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1876)) — answer a direct-message call and the voice view comes up with the other person audible, reliably.
- **Older DM attachments decrypt correctly** ([#1867](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1867)) — images and files in your DM history now use the right key context after key rotations, so your encrypted history stays readable to you.
- **Your DM list shows accurate online status** ([#1874](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1874)) — presence in DM rows now stays in sync with your friends list.
- **Image lightbox polish** ([#1868](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1868)) — enlarged images are properly centered and no longer show a magnifier cursor.

## [0.2.7] — 2026-06-23

> Versions 0.2.4–0.2.6 never reached you — they were release-pipeline iterations that produced no public release. Everything they carried ships here.

### Added

- **Image lightbox viewer** ([#1813](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1813)) — click any image attachment to see it full size, zoom in, and save it with a native Save As dialog.
- **Public invite links** ([#1821](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1821)) — share a server invite link anywhere; it opens a landing page and routes new members straight into the app.
- **Smarter camera quality in group calls** ([#1831](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1831)) — SVC-first camera layer selection means the server sends each viewer the resolution they actually need. Your bandwidth goes to the video you're watching, not the video you aren't.

### Fixed

- **Video recovers after encryption key catch-up** ([#1784](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1784)) — end-to-end-encrypted video used to black-screen after a key epoch desync. It now recovers on its own; you don't have to do anything.
- **Audio output device changes apply mid-call** ([#1823](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1823)) — switch your speaker or headset in settings and live call audio follows immediately.
- **Large batch of chat and UI polish** ([#1798](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1798)–[#1836](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1836)) — DM conversation avatars, DM unread badge and notification suppression while a conversation is open, self-sent DM previews, GIF embeds in DMs, self-mention highlighting, ordered-list numbering, inline code wrapping, bigger click targets for the message composer and user popovers, context-menu and screen-share-picker layering fixes, notification-sound and TTS preview feedback, theme-correct self profile card, friend category manager styling, voice participant frame overlap, active server restored after reload, and blocked partially-uploaded attachment sends.
- **Linux packaging hardening** ([#1801](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1801), [#1825](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1825), [#1863](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1863)) — Concord Voice launches more reliably across Linux desktops, menu icons render correctly in every desktop environment, and RPM packages keep their sandbox permissions intact.
- **Media uploads report clearly when unavailable** ([#1826](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1826)) — when media storage is disabled, the server now says so with a proper "service unavailable" response instead of a confusing error. You deserve a straight answer, even from an error message.

### Security

- **Closed a WebSocket channel permission bypass** ([#1811](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1811)) — channel access rules (roles and channel-level permissions) were enforced on the REST API but not consistently on the real-time WebSocket path. They now hold on both. Stating it plainly: this was a gap, and it's closed.

### Changed

- **Behind-the-scenes tooling, CI, and dependency upkeep** — routine dependency updates (including Electron 42.4.1 and mediasoup 3.20.9), release-pipeline and deployment reliability work, and infrastructure housekeeping.

## [0.2.3] — 2026-06-22

### Added

- **Live message character-limit counter with overflow handling** ([#1709](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1709)) — see exactly how much room you have left as you type. The composer counts characters against your account's message limit and flags over-limit text before you send — no more surprise rejections.

### Changed

- **Behind-the-scenes tooling, CI, and documentation upkeep** ([#1780](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1780), [#1781](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1781), [#1764](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1764), [#1783](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1783)) — build-pipeline and security-scanning maintenance, documentation cleanups, and a routine supply-chain threat-list refresh. Nothing changes in how Concord Voice behaves for you.

## [0.2.2] — 2026-06-22

### Added

- **Premium lock UI + redemption codes** ([#1724](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1724), [#1730](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1730), [#1737](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1737)) — the groundwork for subscriptions: lock badges and gates mark premium features, a universal redemption-code system redeems access, and per-tier audio/video quality limits are enforced server-side when you join voice.
- **Public Known Issues list** ([#1736](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1736), [#1769](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1769)) — a pinned, always-current Known Issues tracker in the public feedback repo, linked straight from the in-app feedback dialog. Check whether we already know about your issue before you file it.
- **"Load latest UI" recovery** ([#1779](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1779)) — a Settings ▸ About button, plus an automatic launch-time retry, that loads the newest app UI without a restart. If a slow network start left you on the built-in fallback, you're one click from current.
- **Admin/ops console authentication** ([#1703](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1703)) — operator console sign-in requires a password plus a WebAuthn hardware-key second factor. A password alone doesn't open the ops door.
- **GIFs load automatically by default** ([#1774](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1774)) — new accounts start with "Load GIFs from KLIPY automatically" turned on. Existing accounts keep the choice you already made — your setting is yours.

### Fixed

- **Voice audio crackle and garbling** ([#1765](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1765), [#1777](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1777)) — small calls no longer clip the first words of speech: the speaker-limit cap now stays out of the way in rooms it was never needed in, and silent (DTX) audio frames pass through correctly instead of garbling the start of speech.
- **Reconnect after server deploys** ([#1770](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1770)) — a brief server-side disconnect now reconnects on its own instead of stranding you at "Reconnecting", and a transient sign-in hiccup no longer wipes a "Remember Me" session. "Remember Me" now means it.

### Changed

- **Behind-the-scenes tooling, CI, documentation, and dependency upkeep** ([#1731](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1731), [#1733](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1733), [#1762](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1762), [#1771](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1771), [#1772](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1772), [#1775](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1775), [#1776](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1776)) — feedback-triage automation, release-mirror pipeline fixes, a security-list refresh, doc cleanups, and audio diagnostics that are opt-in and off by default — diagnostics run only when you turn them on.

## [0.2.1] — 2026-06-20 (macOS DMG installer delivery)

> Patch release. v0.2.0-Beta shipped the macOS client as a `.zip` only — the
> signed-and-notarized `.dmg` installer was built, signed, and notarized by CI
> but never attached to the GitHub Release because the release-asset glob
> omitted `*.dmg`. This release corrects the release pipeline so the branded
> drag-to-Applications `.dmg` ships alongside the `.zip` (which remains the
> electron-updater auto-update artifact — the `.dmg` is install-only).

### Fixed

- **macOS `.dmg` installer now attached to releases** ([#1722](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1722)) — the release-asset `find` glob in `build-desktop.yml` (feeding `gh release create`) omitted `*.dmg`, so the notarized installer was missing from the v0.2.0 GitHub Release and the public mirror. The DMG is now attached and normalized to `ConcordVoice-<version>-macos-<arch>.dmg`, consistent with the `.zip`. The `latest-mac.yml` auto-updater manifest deliberately remains `.zip`-only (Squirrel.Mac cannot auto-update from a DMG).

## [0.2.0-Beta] — 2026-06-20 (Phase 2 — Beta release)

> Release-level rollup of Phase 2A + Phase 2B work. Per-revision detail lives in the `[0.1.12]`–`[0.1.18]` entries below; this entry surfaces the user-visible themes that close the v0.2.0-Beta milestone.

### Added

- **Federated identity — Google SSO** ([#808](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/808)) — backend OAuth flow for Google sign-in and registration; desktop client integration shipped alongside Apple SSO in [#824](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/824) (`client/desktop/src/main/ssoLoopback.ts`, `Login.tsx` / `Register.tsx`).
- **Federated identity — Apple Sign in with Apple** ([#824](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/824)) — privacy-relay-aware Apple SSO alongside Google.
- **MFA / WebAuthn authentication** ([#202](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/202)) — TOTP, WebAuthn/FIDO2, backup codes, recovery circles, trusted devices; closes #89.
- **Account erasure (GDPR right to be forgotten)** ([#717](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/717)) — `POST /api/v1/privacy/erase-account` wired to a transactional account-deletion service that cascades across all linked tables; `refresh_tokens.user_id` cascades atomically.
- **Account recovery** ([#328](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/328)) — zero-knowledge key recovery flow.
- **RBAC / SBAC permissions system** ([#242](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/242)) — granular role-based access control with audit logging; closes #82. Context menu wired to roles in [#548](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/548).
- **Object storage on MinIO** ([#325](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/325)) — user image assets migrated from PostgreSQL to S3-compatible storage with two-tier media access; closes #166.
- **Server ownership transfer** ([#351](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/351)) — full lifecycle with MFA, email confirmation, reversal tokens; closes #244.
- **Email verification on registration** ([#273](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/273)) — SMTP-based verification; later migrated from Proton SMTP to Resend with branded templates and `verify.concordvoice.chat` subdomain.
- **Pending registrations** ([#688](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/688)) — registration creates `pending_registrations` with a 15-minute TTL; closes #527 and #621.
- **Chat enhancements (#168 series)** — message reactions ([#459](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/459)), reply / quote ([#463](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/463)), pinning ([#465](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/465)), E2EE-native search ([#468](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/468)), file & image attachments ([#470](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/470)), draft persistence ([#477](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/477)), desktop notifications ([#478](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/478)), keyboard shortcuts ([#479](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/479)), group DMs ([#472](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/472)), extended Markdown rendering ([#711](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/711)).
- **Klipy GIF integration** ([#557](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/557)) — GIF search, picker, and privacy proxy through the control-plane; theme-aware logos and disclaimers.
- **Server-enforced mute and deafen** ([#546](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/546)) — server-side mute/deafen state propagated to the mediasoup SFU; consumers paused/resumed at SFU level.
- **@mention notification routing in E2EE channels** ([#310](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/310)) — server-side mention detection without decryption.
- **DM key-epoch enforcement** ([#298](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/298)) — key revocation table and epoch checking in the WebSocket path; closes #122.
- **Channel-key rotation on member removal** — E2EE forward secrecy for channel keys; closes #96.
- **Desktop auto-updater** ([#155](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/155), [#381](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/381)–[#387](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/387)) — safe updates with rollback, branded splash screen, fill-progress, error states, position memory, and structured logging.
- **Server-proxied desktop updates** ([#264](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/264)) — privacy-first update delivery with no per-client telemetry.
- **SPA deployment pipeline** ([#429](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/429)) — file server, versioning, and GitHub Actions workflow for hot-update SPA bundles. SPA deploy contract added in [#773](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/773) coupling bundle-hash and handler-path.
- **Bundled-SPA fallback (Option C)** ([#831](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/831), [#832](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/832), [#835](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/835)) — desktop client falls back to the bundled SPA on hot-update failure with the `app://` scheme, an Option C user-facing overlay, and IPC v9.
- **WebSocket reconnect race fix + subscribe-barrier protocol** ([#769](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/769)) — closes #752.
- **Self-hosted coturn STUN/TURN** ([#124](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/124)) — infrastructure for NAT traversal; cert isolation and `turn.concordvoice.chat` SAN added in [#577](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/577).
- **Public Tier-1 media proxy** ([#570](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/570)) — unauthenticated access for public media assets.
- **Token theft detection** ([#89](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/89)) — machine ID + IP binding with automatic revocation. Sessions capture real client IP via trusted-proxy CIDR allowlist in [#702](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/702).
- **Proactive token refresh** ([#329](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/329)) — main-process JWT refresh before expiry; closes #240 and #254.
- **Profile and identity asset theming** ([#251](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/251), [#252](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/252)) — per-user theming, profile cards, DM sidebar cards, avatar theming.
- **Image crop editors for profile and server images** ([#357](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/357)).
- **Username restrictions, period support, yearly change cooldown** ([#330](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/330)).
- **OS-level permission management** ([#321](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/321)) — request, check, and enforce system permissions; closes #197.
- **Friend requests** ([#203](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/203)) — accept/decline UI with context menu.
- **Notification sounds** for chat and voice events ([#375](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/375), [#394](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/394)); per-category sound volumes ([#743](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/743)); DM call sounds with looping ([#554](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/554)).
- **Developer Mode toggle** ([#567](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/567)) — DevTools accessible in Alpha/Beta builds only via the developer mode setting.
- **Code signing — macOS** ([#641](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/641)) — Developer ID Application cert wired to sign and notarize macOS builds.
- **Code signing — Windows** ([#649](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/649)) — Microsoft Artifact Signing for `Setup.exe`; closes #404.
- **Docker network segmentation** ([#442](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/442)) — service isolation, request-ID propagation, Redis auth bans.
- **CI/CD pipeline** ([#128](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/128), [#130](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/130)) — GitHub Actions `build.yml` with parallel test, coverage, and SonarQube; later hardened with Semgrep SAST in [#457](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/457).
- **Shai-Hulud 2.0 supply-chain IOC scanner** ([#722](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/722)) — closes #715; IOC list refreshed in [#781](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/781).
- **AI governance framework** ([#454](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/454)–[#458](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/458), [#500](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/500)–[#522](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/522)) — AI-generated code policy, CODEOWNERS, agentic controls, Semgrep SAST, path-scoped internal AI-assistant rules, Claude Code skills, custom agents, Copilot prompt templates, MCP project config.

### Changed

- **React 18 → 19.2.4** ([#181](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/181)).
- **react-router-dom 6 → 7.13.1** ([#182](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/182)).
- **Zustand 4 → 5.0.12** ([#183](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/183)).
- **ESLint → 10 flat config** ([#184](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/184), [#185](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/185), [#186](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/186)).
- **Vite 7 → 8.0.0** ([#287](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/287)) plus `@vitejs/plugin-react` 4 → 6.
- **Go 1.24 → 1.26.1** ([#193](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/193)) with `govulncheck` hardening.
- **Electron 33 → 41.x**, **mediasoup 3.13 → 3.19.18**, **mediasoup-client → 3.18.7**, **TypeScript 6.0.2**, **typescript-eslint 8.58**.
- **E2EE password-derived key** — PBKDF2 → Argon2id client-side ([#117](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/117)).
- **macOS notarization** switched to App Store Connect API key ([#826](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/826)).
- **Cognitive complexity reduction** across Go control-plane handlers and TypeScript frontend / media-plane ([#418](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/418), [#419](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/419), [#498](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/498), [#505](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/505), [#550](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/550)–[#553](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/553)).
- **Documentation audit** completed for the v0.2.0-Beta release gate ([#214](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/214)) — drift inventory tracked internally; PR-1 verification merged in [#823](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/823).
- **MCP server deployment refactored into per-host configs** ([#838](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/838)) — closes #778. Splits into `.mcp.json` (Claude Code CLI / App via `launchctl setenv`) and `.vscode/mcp.json` (VS Code native MCP via `${input:VAR}` → secret store). Eliminates `launchctl setenv` exposure for VS Code native MCP — OWASP A02 (Security Misconfiguration) win. Policy doc (`docs/policies/mcp-server-policy.md`) rewritten with the three-surface credential taxonomy.

### Removed

- **Sentry telemetry** stripped from all three services ([#770](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/770) control-plane, [#780](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/780) media-plane, [#793](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/793) Electron client) plus a closing sweep through MCP, CI, and rules ([#796](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/796)). The Sentry MCP server config was finally removed in the per-host MCP cleanup ([#838](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/838), closing the MCP-config dimension). Closes #610, #614, #672. The integration that landed earlier in the cycle (#586, #622, #623, #668, #682) was reversed once telemetry surfaced zero production logs and forced re-consent friction; project memory `Sentry — being removed` documents the decision.
- **Postgres MCP server config** removed alongside Sentry MCP ([#838](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/838)) — unused operationally; final state is 6 servers in `.mcp.json` + 6 in `.vscode/mcp.json`.
- **Deprecated `WebSocketMessage` source-compat type alias** removed from `client/desktop/src/renderer/services/websocketService.ts` ([#1185](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/1185)) — the discriminated-union migration ([#709](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/709) / PR [#1184](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1184)) made the shim unused. `WebSocketEvent` remains the canonical name.

### Fixed

- **E2EE channel-key OperationError** with structured diagnostic envelope ([#765](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/765)).
- **E2EE voice / video codec collision** — WebRTC BUNDLE misrouting between consumers ([#291](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/291), [#292](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/292), [#293](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/293)).
- **E2EE Insertable Streams → RTCRtpScriptTransform** migration ([#355](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/355)).
- **E2EE key request flood** — session-scoped cache plus CI hardening ([#241](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/241)).
- **E2EE frame decryption** recovery and CSK rotation hardening ([#232](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/232), [#284](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/284)).
- **DM key-epoch enforcement, presence sync, and key distribution** ([#298](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/298)).
- **Replied-to message decryption** on REST message fetch (not only WebSocket) ([#542](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/542)).
- **DM thread real-time preview and reorder** ([#541](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/541)) — closes #486.
- **Message editing** in E2EE channels and DMs ([#220](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/220)).
- **Voice and video** — black-screen recovery, codec selection, screen-share audio, audio persistence on navigation ([#198](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/198), [#227](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/227), [#295](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/295), [#299](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/299), [#396](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/396)).
- **Hub goroutine races** with test cleanup — flaky voice tests resolved ([#476](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/476)); deterministic channel-based sync replaced `time.Sleep` in WebSocket hub tests ([#544](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/544)).
- **WebSocket reconnect** — connection-lost handler replaced page-reload with direct WS reconnect ([#194](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/194), [#439](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/439)).
- **Self-user shown as Offline despite an active connection** — Member List, UserPopover, and profile now reconcile `selfStatus` from the connect-time presence snapshot ([#1535](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1535)) — closes #803.
- **Postgres "invalid length of startup packet" flood** ([#779](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/779)) — closes #755.
- **PiP child window** loads SPA route, not marketing site ([#815](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/815)) — closes #802.
- **PiP window signaling and local user identification** ([#415](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/415)).
- **Desktop login on bundled SPA fallback** uses `app://` scheme ([#832](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/832)) — closes #830.
- **MFA verify response** parsing — `access_token` extracted from `/mfa/verify` ([#814](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/814)).
- **electron-updater trust path** hardening on macOS / Windows / CI ([#655](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/655)).
- **Build-desktop CI ASAR integrity** verification ([#686](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/686)) — closes #683.
- **Preload bundling with esbuild** for sandbox compatibility ([#678](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/678)).
- **MFA encryption key wiring** through Docker Compose ([#225](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/225)).
- **Klipy GIF media proxy 401** — webRequest auth injection ([#687](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/687)); proxy routes nested under `/gifs` ([#580](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/580)).
- **Klipy GIF rendering** — envelope unwrapped on every decrypt path; nested rendition shape parsed correctly ([#566](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/566)).
- **NATS server config and coturn TLS / external-IP** ([#576](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/576), [#577](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/577)).
- **Modal nested Escape handler** firing on all stacked modal instances ([#480](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/480)).
- **Server role styling** isolated from DM message rendering ([#543](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/543)).
- **Accessibility pass** — semantic HTML, keyboard navigation, ARIA, UI polish ([#380](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/380), [#427](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/427), [#482](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/482)).
- **Theme markdown syntax help modal and help icon** ([#748](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/748)).
- **Cloudflare beacon verification** + `/spa/` nginx route + defensive sentinel ([#766](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/766)) — closes #750.

### Security

- **`Error.cause` propagation closed** ([#714](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/714)) — `console.error` / `console.warn` no longer pass raw `Error` arguments through main-process logs; ESLint enforcement and a Vitest regression test added.
- **Token-fingerprint leaks removed** ([#704](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/704)) — 10 token-suffix leaks in `tokenManager.ts` removed; ESLint warnings remediated and security rules promoted to error.
- **External-link scheme tightened** ([#774](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/774)) — `setWindowOpenHandler` and `will-navigate` restricted to `https:`-only with ESLint drift defense; user-initiated `open-external` IPC retains the broader `http:` / `https:` / `mailto:` policy. Closes #754.
- **electron-updater TLS certificate pinning** on `api.concordvoice.chat` ([#719](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/719)).
- **nginx hardening** — H2C smuggling vector closed; Host header injection blocked ([#525](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/525)).
- **GitHub Actions shell injection** resolved (Semgrep) ([#523](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/523)).
- **CORS hardening** — null/empty origin rejection, custom header validation ([#259](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/259)).
- **Hardcoded dev credentials removed**, production guards added ([#260](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/260)).
- **Scanner hardening** — brute-force probe mitigation at infrastructure level ([#153](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/153)); production infrastructure hardened against vulnerability scanners ([#189](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/189)).
- **Dependabot vulnerability fixes** — npm overrides for transitive vulnerabilities ([#289](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/289), [#314](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/314)).

### Known Issues

Tracking at time of v0.2.0-Beta release. For the full open-issue list, see [issues](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues).

- **[#817](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/817) — "No Internet" Retry button stuck after network restore** — after the desktop client trips the no-internet dialog, the Retry button does not visibly progress once connectivity is back; the user must use Exit App to break out and relaunch. Workaround: quit and relaunch the desktop client once the network is restored.
- **[#807](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/807) — Markdown rendering correctness in chat** — H1 renders smaller than H2, fenced code blocks parse incorrectly in some inputs, and vertical spacing is heavier than expected. Cosmetic only; message content is preserved. Workaround: none required for delivery; fix tracked for v0.2.x.
- **[#805](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/805) — Pinned Messages panel drops media and shows GIFs as raw JSON** — pinned messages with image attachments lose the image; pinned KLIPY GIFs render as the raw envelope text. The original message in the main chat is unaffected. Workaround: scroll to the source message in the channel for the full rendering.
- **[#804](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/804) — KLIPY GIF picker hits 429 during normal scrolling** — the shared rate limiter for the GIF media proxy and the API endpoint is too aggressive when ~30 picker tiles fan out simultaneously. Workaround: pause briefly between scrolls in the GIF picker; the limiter resets quickly.
- **[#799](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/799) — Member List "+ Add Role" dropdown clipped inside the card** — the role-picker dropdown is constrained by its container and adds an inner scrollbar instead of overflowing the card. Workaround: scroll inside the Member List card to access roles below the fold.
- **[#707](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/707) — Profile editor loses in-progress edits on background user updates** — `ProfileInfoForm`'s reset effect re-runs on any user object mutation, which can wipe unsaved field edits if the underlying user object refreshes mid-edit. Workaround: save profile changes promptly; avoid leaving the editor open while presence or other user-object events fire.

### Migration from v0.1.0-Alpha

**No end-user breaking changes.** New features (Apple/Google SSO, MFA/WebAuthn, channel-key revocation, server mute/deafen, Klipy GIF integration, DM message pinning, account erasure, bundled-SPA fallback) are additive — existing v0.1.0-Alpha installations continue to work without manual intervention.

**Automatic migrations applied on first connection / first server run:**

- Database schema additions (migrations 000054–000061): server mute/deafen state, Klipy GIF customer IDs, DM message pinning index, pending registrations TTL, account-deletion cascade, removed `sentry_delete_attempted` column, and SSO identities (`is_relay_email` for Apple privacy-relay handling).
- Refresh-token cascade (migration 000059): `refresh_tokens.user_id` becomes `ON DELETE CASCADE`, which strengthens the atomic-revocation invariant for account erasure.
- E2EE key derivation: PBKDF2 → Argon2id ([#117](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/117)) migrates transparently on next login — legacy keys are unwrapped with PBKDF2, re-wrapped with Argon2id, and uploaded to the server. No user action required; migration 000034 adds `key_derivation_alg` tracking.
- Renderer migrations: existing local data migrates client-side via the standard Zustand `persist` migration path on first launch; no user-visible state loss.

**Behavior changes that may surprise:**

- **External link policy** ([#774](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/774), [#775](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1172)): the Electron client's `setWindowOpenHandler` and `will-navigate` now restrict to `https:` only — passive navigation (redirects, programmatic `window.open`) cannot escape to the OS browser for non-`https:` schemes. User-clicked links in `UserProfileModal` and the Markdown pipeline (`SafeLink.tsx`) route through the `open-external` IPC handler, which accepts `{http, https, mailto}` because the explicit click is consent. Legacy `http://` profile links continue to work via this path.
- **Token revocation on password change**: refresh tokens are now atomically revoked when a user changes their password (was a known gap in Alpha). Active sessions on other devices will be logged out on the next refresh attempt. This is the intended behavior; surfaced here because v0.1.0-Alpha did not enforce it.
- **Sentry telemetry removed**: zero behavior change to end users (telemetry was opt-in and zero events were captured in production); flagged for transparency.

**For self-hosted operators:**

- Review the migration set `services/control-plane/migrations/` (000054 onward represents the Alpha→Beta delta) for any deployment-specific actions.
- The MCP server cleanup ([#838](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/838)) is dev-environment-only — affects contributor tooling, not production deployment. No action required for self-hosted operators.
- For Apple sign-in support, configure the Apple Sign In credentials per Apple Developer documentation: Team ID, Key ID (with the corresponding `.p8` private key), Services ID, and the loopback redirect URI. Set the corresponding env vars in your control-plane `.env` (see `services/control-plane/internal/oauth/` for the variable names; the source-of-truth is `apple_clientsecret.go`). Without these, the Apple SSO button will be visible but sign-in attempts will fail.
- macOS code signing now uses App Store Connect API key ([#826](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/826)) — only relevant if you build your own signed binaries; the public release builds remain Apple Developer ID signed.

---

## [0.1.41] — 2026-05-20 (#806 cross-platform window chrome)

### Added

- **Cross-platform native window controls** — Windows + Linux now show native close / minimize / maximize buttons in the top-right via Electron's `titleBarOverlay` API; macOS retains its native traffic lights via `titleBarStyle: 'hiddenInset'`. The per-platform branching lives in `client/desktop/src/main/browserWindowConfig.ts` ([#806](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/806)).
- **Branded Titlebar** — new `<Titlebar />` component renders centered `CONCORD VOICE` in BaronNeue.woff with the running version + active SPA hash (`v0.1.41-abc123`). The version line updates live when an SPA hot-update lands via the new `spa:versionChanged` IPC event.
- **Window state persistence** — size + position + maximized state save to `window-state.json` under `app.getPath('userData')` with 500ms debounce on resize/move and synchronous write on close. Restore validates the saved bounds against the current display layout (4 safety checks: NaN/missing, display intersection, min/max size, negative-coords). Wayland sessions omit x/y per compositor-controlled placement.
- **Client Behavior settings** — new Settings → Appearance subsection lets users assign the `[×]` close and `[—]` minimize buttons to system tray, OS taskbar/dock, or graceful quit. Mutex + coverage rules visualize invalid configs as greyed-out segmented-control cards with explanatory `title=""` tooltips. Dynamic explanation panel reads the current configuration and renders 3 "How do I X" paragraphs.
- **Coordinated-pair sibling [#1099](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/1099)** — system tray icon. v0.1.41 ships the Client Behavior surface; #1099 ships the tray icon. Both required for the `[X] → tray` and `[-] → tray` paths to be user-visible.

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

- **Linux build no longer fails at `@reforged/maker-appimage` packaging** — commit [21accce5](https://github.com/Concord-Voice/Concord-Voice-Alpha/commit/21accce5) ("Change company name to 'Concord Voice LLC'") silently bundled an unrelated `packagerConfig.executableName` rename from `'concord-voice'` to `'Concord Voice'` alongside its legitimate company-name updates. With executableName changed to `'Concord Voice'`, the packaged Linux binary became `Concord-Voice-linux-<arch>/Concord Voice` (with a space), but the Linux makers' `bin: 'concord-voice'` option still performed a literal-string file lookup for `concord-voice`, failing with `"Could not find executable 'concord-voice' in packaged application"` ([run 26148494680](https://github.com/Concord-Voice/Concord-Voice-Alpha/actions/runs/26148494680)). The bug sat latent on `main` for ~6 hours while the cascade-skip regression (since-fixed by [#1077](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1077)) was still suppressing the build matrix, and surfaced on the first `push:main` after the cascade-skip fix shipped — exactly the verification path documented in ADR-0004 (desktop release contract) Invariant 3. Fixed by making `executableName` per-platform: Linux falls back to kebab-case `'concord-voice'` (matching the maker `bin:` lookup and debian-policy §5.6.7), while macOS / Windows retain the proper-name format `'Concord Voice'` (visible in Activity Monitor, Task Manager, and crash reports).

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
- **Per-target notification mute preferences** ([#985](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/985)) — closes [#84](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/84); independent mute toggles for messages vs voice on a per-server/per-DM basis.

### Changed (carried over from unshipped v0.1.36–v0.1.38)

- **Backend stops emitting `is_encrypted` field across API and WebSocket envelopes** ([#1042](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1042)) — the field is now structural (every room/channel is encrypted under [#201](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/201)). Inbound WebSocket envelopes lacking `key_version >= 1` are rejected via close frame 4400 `missing_or_invalid_key_version`. Landed on main in the v0.1.37 bump; first released here.
- **Frontend stops reading `is_encrypted`** ([#1031](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1031)) — Child A of the #201 epic; landed on main in the v0.1.36 bump; first released here.
- **Media-plane removes `is_encrypted` field + documents SRTP-mandatory** ([#1032](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1032)) — Child D of #201; landed on main in the v0.1.37 bump; first released here.

### Fixed (carried over from unshipped v0.1.36–v0.1.38)

- **WebSocket first-attempt drop is now silent + WS auth ticket redacted from console** ([#1046](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1046)) — noisy reconnect log on the very first connection has been quieted; auth ticket no longer surfaces in DevTools console. Landed on main in the v0.1.38 bump; first released here.

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
