# Changelog

All notable changes to Concord Voice will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [0.2.22] — 2026-07-02

### Added

- **Post-update changelog modal** ([#2034](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2034)) — after a desktop update installs and the app restarts, a once-per-version "What's new" dialog summarizes the releases since the version previously installed on that machine. Dismissible, keyboard-accessible, works offline, and never blocks startup.
- **Changelog discipline** ([#2034](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2034)) — this file is now the canonical public change record: entries backfilled for every release since Beta, and a CI gate ensures every future version bump ships with a matching entry.

## [0.2.21] — 2026-07-02

### Added

- **Higher profile and server image upload limits for subscribers** ([#2010](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2010)) — avatar, banner, and server-icon upload size caps now scale with subscription tier.

### Changed

- **Internal tooling, CI, code-quality, and documentation maintenance** ([#2016](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2016), [#2017](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2017), [#2019](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2019), [#2025](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2025)).

### Fixed

- **Server message pinning restored** ([#2011](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2011)) — pinning works again in server channels, and pinned media no longer overflows its panel.
- **Chat scrolling no longer jumps from stale scroll restores** ([#2008](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2008)) — switching channels quickly no longer snaps the view to an outdated position.
- **DM and notification previews show friendly media labels** ([#2015](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2015)) — image and attachment previews read as "Photo"-style labels instead of raw placeholders.
- **Message composer polish** ([#2014](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2014)) — the composer menu now renders above the placeholder text instead of behind it.
- **Fewer unnecessary permission requests** ([#1990](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1990), [#2009](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2009)) — the client no longer issues forbidden channel-override fetches or speculative mention-permission checks it isn't entitled to.
- **Unified desktop update checks** ([#2013](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2013)) — the app's UI and binary update checks now go through one consistent path.
- **Cleaner account-creation identity hint** ([#2018](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2018)) — the suggested identity during sign-up no longer carries an unwanted "@concordvoice.chat" suffix.

### Security

- **Windows install-time update verification armed** ([#2023](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2023)) — the update manifest now carries the expected publisher name, so Windows verifies the code signature of downloaded updates before installing.
- **Public release attestations for mirrored desktop assets** ([#1988](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1988)) — every publicly mirrored desktop release asset now carries verifiable build provenance.
- **Client-attestation activation blockers cleared** ([#2024](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2024)) — corrected the attestation verification default and added a fail-loud guard so misconfiguration cannot pass silently.

## [0.2.20] — 2026-06-30

### Added

- **Self-hosted servers now unlock full entitlements** ([#1985](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1985)) — operators running their own Concord Voice instance get premium-tier media and feature entitlements out of the box, without a subscription.
- **Legal documents in the About page** ([#1987](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1987)) — license, terms, and other legal notices are now viewable directly from Settings ▸ About.

## [0.2.19] — 2026-06-30

### Added

- **Self-hosted desktop login routing** ([#1982](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1982)) — first slice of self-hosted server support: the desktop login flow can route authentication to an operator-run Concord Voice server instead of the managed service.

### Fixed

- **Desktop update recovery via the public feed** ([#1984](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1984)) — the app now checks the public release feed for updates, so clients stranded on an older version can recover and receive new releases reliably.

### Changed

- **Internal tooling, CI, and deployment configuration maintenance.**

## [0.2.18] — 2026-06-29

### Security

- **Restored Cloudflare certificate pinning** ([#1983](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1983)) — the desktop client's certificate pinning for Cloudflare-fronted connections had lapsed; this release recovers it so connections to Concord services are again verified against the expected certificates.

## [0.2.17] — 2026-06-29

### Added

- **Reactions on direct messages** ([#1976](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1976)) — you can now react to DM messages with emoji, matching the reactions already available in server channels.

### Changed

- **Internal tooling, CI, and dependency maintenance** ([#1979](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1979)).

### Fixed

- **More reliable connection at startup** ([#1978](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1978)) — eliminated a race in the WebSocket authentication handshake that could leave the client disconnected right after launch.
- **Restored connectivity after server certificate rotation** ([#1980](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1980)) — the client now trusts the rotated production API certificate pin, fixing failed connections for clients that enforced the old pin.

## [0.2.16] — 2026-06-29

### Added

- **Notification content privacy preference** ([#1970](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1970)) — new setting to control how much message content appears in desktop notifications, so previews can be hidden on shared or public screens.
- **Custom themes are now free** ([#1974](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1974)) — custom theme creation is available to all users, no subscription required.

### Fixed

- **Voice audio no longer drops when navigating to DMs** ([#1972](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1972)) — the voice audio pipeline stays mounted while browsing direct messages, so call audio keeps playing during navigation.
- **macOS app now relaunches after installing an update** ([#1973](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1973)) — "Restart to update" previously quit the app without reopening it on macOS; the update-install restart path now brings the app back up.

### Changed

- **Internal tooling, CI, and supply-chain maintenance** ([#1968](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1968), [#1969](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1969)) — retired a legacy automation credential and automated the supply-chain indicator-list refresh guard; no user-facing impact.

## [0.2.15] — 2026-06-29

### Added

- **Member timeout moderation** ([#1967](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1967)) — moderators can now temporarily time out server members, restricting their participation for a set duration.
- **macOS Applications-folder move prompt** ([#1966](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1966)) — on first launch outside /Applications, the app offers to move itself to the Applications folder for a cleaner install and reliable updates.

### Changed

- **Internal tooling, CI, and deployment maintenance.**

### Security

- **Stricter passkey registration requirements** ([#1963](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1963)) — WebAuthn registration now requires passkey-grade platform authenticator options, ensuring new passkeys meet the intended security bar.
- **Access token revoked on account erasure** ([#1965](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1965)) — erasing an account now immediately invalidates the current access token, so no authenticated session survives the deletion.

## [0.2.14] — 2026-06-28

### Added

- **Per-channel audio quality standard** ([#1946](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1946)) — voice channels can now set a shared audio quality tier that applies to every member, bounded by the server's tier, with a new slider in channel settings.
- **SVC / Simulcast casting toggles** ([#1929](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1929)) — advanced video controls in Settings ▸ Audio & Video let you choose which layered-video codec modes your client will publish with.
- **Self-hosted TLS certificate provisioning** ([#1919](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1919)) — self-hosted operators can provision an origin certificate via self-signed, Let's Encrypt, or bring-your-own modes with one script.

### Changed

- **My Profile moved into Settings ▸ Account** ([#1930](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1930)) — profile editing now lives in Settings; the old quick-link deep-links to the same place. SSO controls on the Security page were also polished ([#1951](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1951)).
- **Internal tooling, CI, and dependency maintenance** — supply-chain threat-list refreshes, deploy-summary and mirror-sync fixes, and dev-environment upkeep.

### Fixed

- **Username case-handling consistency** ([#1936](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1936)) — usernames are now case-insensitive everywhere, fixing an SSO mixed-case profile-edit lockout, duplicate accounts differing only by case, and case-sensitive friend-add lookups.
- **macOS notification permission status** ([#1960](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1960)) — the app now correctly reconciles its notification settings with the actual macOS permission state.
- **Self-hosted TLS failures no longer silent** ([#1964](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1964)) — local certificate provisioning errors are now surfaced to the operator instead of being swallowed.

### Security

- **Hardened email MFA setup for production** ([#1961](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1961)) — tightened the email-based multi-factor enrollment flow.
- **Cached-UI origin allowlisted ahead of activation** ([#1928](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1928)) — the signed offline UI cache's origin was added to server allowlists so passkey login and voice keep working when that fallback activates.

## [0.2.13] — 2026-06-28

### Fixed

- **Send-to-a-Friend invite bubbles now show the correct sender** ([#1909](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1909)) — invite messages in chat were attributed to the wrong user; they now display the person who actually sent the invite.
- **System Permissions settings are clearer and easier to find** ([#1910](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1910)) — improved wording and navigation discoverability for the System Permissions section in Settings.

### Security

- **Linux updates are now signature-verified before install** ([#1923](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1923)) — AppImage, deb, and rpm update artifacts carry detached Ed25519 signatures that the client verifies against a bundled public key; an update that fails verification is refused.
- **Signed offline UI cache activated** ([#1907](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1907), [#1908](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1908)) — the desktop client can now fall back to a cryptographically verified last-known-good copy of the app UI, checked against an embedded public key before use.

### Changed

- **Internal tooling, CI, and configuration maintenance** ([#1918](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1918), [#1920](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1920)) — cleanup of a retired sign-in configuration value across internal checks and documentation.

## [0.2.12] — 2026-06-27

### Fixed

- **Camera and screen share no longer cut out when video quality layers switch** ([#1903](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1903)) — re-negotiating video layers mid-call could stop the underlying capture track, killing every camera in the room; producers now keep the track alive across the switch.
- **Age-verification status now restores correctly after a reload** ([#1904](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1904)) — verified state is re-fetched from the server on startup, so already-verified users are no longer re-prompted.

### Changed

- **Internal tooling, configuration, and code-quality maintenance** ([#1900](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1900), [#1901](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1901), [#1906](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1906)) — engineering plan write-ups, a storage-configuration rename groundwork change, and static-analysis cleanup; no user-facing behavior changes.

## [0.2.11] — 2026-06-26

### Fixed

- **Hardware video encoding now uses your GPU's actual capabilities** ([#1879](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1879)) — the client now queries the system's supported hardware encode profiles instead of assuming a fixed codec set, so camera and screen-share encoding picks codecs your GPU can genuinely accelerate.

## [0.2.10] — 2026-06-26

### Added

- **Self-hosted installer and lifecycle CLI** ([#1892](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1892)) — a guided installer for running your own Concord Voice instance, plus a `concord-selfhost` command for starting, stopping, monitoring, and checking the health of the stack.

### Fixed

- **AV1 video black screens in end-to-end-encrypted calls** ([#1896](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1896)) — AV1 camera and screen-share video could fail to decrypt and render as a black screen; per-frame media encryption was reworked (frame crypto v4) so AV1 streams now decrypt reliably.

### Changed

- **Internal tooling, CI, and dependency maintenance** ([#1877](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1877), [#1894](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1894)) — refreshed supply-chain threat indicators and made macOS release builds retry a transient DMG packaging flake.

## [0.2.9] — 2026-06-24

### Added

- **Signed offline UI cache** ([#1880](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1880)) — the desktop app now keeps a cryptographically signed last-known-good copy of the latest UI, so it can start with the current interface even when the update server is briefly unreachable.
- **Server capabilities discovery endpoint** ([#1883](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1883)) — new `GET /api/v1/server/capabilities` lets clients discover what a (self-hosted) server supports before connecting.

### Fixed

- **Video no longer black-screens after a mid-call key rotation** ([#1885](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1885)) — encrypted video frames now carry their channel-key version, so receivers pick the right decryption key after an end-to-end-encryption key rotation instead of dropping to a black screen.
- **Public release mirroring now triggers only on real releases** ([#1887](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1887)) — corrected the condition that publishes signed builds to the public downloads page.

### Security

- **Stricter validation of the API server address in packaged builds** ([#1882](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1882)) — the app now only accepts the official service address or a self-hosted server that passed the app's own verification probe, hardening against a compromised UI redirecting the client.

### Changed

- **Internal tooling and engineering-workflow maintenance** ([#1884](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1884), [#1888](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1888)).

## [0.2.8] — 2026-06-24

### Changed

- **Refreshed invite landing page** ([#1869](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1869)) — restyled the page shown when opening a server invite link.
- **Internal tooling, CI, and test maintenance** ([#1864](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1864), [#1871](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1871), [#1875](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1875)) — including expanded automated color-contrast (WCAG) checks.

### Fixed

- **App UI updates arrive reliably, and sessions survive a soft reload** ([#1872](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1872)) — the client now retries fetching the latest UI after a cold-start network hiccup, checks for newer UI automatically (deferring while you're in a call), and keeps session-only ("Remember Me" off) sign-in and encryption keys across an in-app reload without ever writing them to disk.
- **Encrypted video stays in sync with layered/simulcast streams** ([#1865](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1865)) — hardened media end-to-end-encryption key epochs so camera and screen-share video no longer desyncs or black-screens with layered video.
- **Live audio device switching** ([#1866](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1866)) — changing your microphone or speaker during a call now retargets the active audio streams correctly.
- **DM voice calls open the call view and play remote audio** ([#1876](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1876)) — answering a direct-message call now reliably brings up the voice view with the other person audible.
- **Older DM image and file attachments decrypt correctly** ([#1867](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1867)) — attachments in DM history now use the right key context after key rotations.
- **DM list shows accurate online status** ([#1874](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1874)) — presence in DM rows now stays in sync with your friends list.
- **Image lightbox polish** ([#1868](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1868)) — enlarged images are properly centered and no longer show a magnifier cursor.

## [0.2.7] — 2026-06-23

> Versions 0.2.4-0.2.6 were version bumps that never produced a public release (release pipeline iterations); their changes ship in this release.

### Added

- **Image lightbox viewer** ([#1813](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1813)) — click any image attachment to open a full-size viewer with zoom and a native Save As dialog.
- **Public invite links** ([#1821](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1821)) — shareable server invite links that open a landing page and route new members into the app.
- **Smarter camera quality in group calls** ([#1831](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1831)) — SVC-first camera layer selection lets the server deliver each viewer the video resolution they actually need, saving bandwidth in larger calls.

### Fixed

- **Video recovers after encryption key catch-up** ([#1784](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1784)) — end-to-end-encrypted video no longer black-screens after a key epoch desync; the stream now recovers automatically.
- **Audio output device changes apply mid-call** ([#1823](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1823)) — switching your speaker/headset in settings now retargets live call audio immediately.
- **Large batch of chat and UI polish** ([#1798](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1798)–[#1836](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1836)) — DM conversation avatars, DM unread badge and notification suppression while a conversation is open, self-sent DM previews, GIF embeds in DMs, self-mention highlighting, ordered-list numbering, inline code wrapping, bigger click targets for the message composer and user popovers, context-menu and screen-share-picker layering fixes, notification-sound and TTS preview feedback, theme-correct self profile card, friend category manager styling, voice participant frame overlap, active server restored after reload, and blocked partially-uploaded attachment sends.
- **Linux packaging hardening** ([#1801](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1801), [#1825](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1825), [#1863](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1863)) — more reliable launch on Linux desktops, correct menu icons across desktop environments, and preserved sandbox permissions in RPM packages.
- **Media uploads report clearly when unavailable** ([#1826](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1826)) — the server now returns a proper "service unavailable" response instead of a confusing error when media storage is disabled.

### Security

- **Closed a WebSocket channel permission bypass** ([#1811](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1811)) — channel access rules (roles and channel-level permissions) are now enforced consistently on the real-time WebSocket path, not just the REST API.

### Changed

- **Internal tooling, CI, and dependency maintenance** — routine dependency updates (including Electron 42.4.1 and mediasoup 3.20.9), release-pipeline and deployment reliability work, and infrastructure housekeeping.

## [0.2.3] — 2026-06-22

### Added

- **Live message character-limit counter with overflow handling** ([#1709](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1709)) — the message composer now shows a character counter driven by your account's message limit and clearly flags over-limit text before you send.

### Changed

- **Internal tooling, CI, and documentation maintenance** ([#1780](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1780), [#1781](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1781), [#1764](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1764), [#1783](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1783)) — build-pipeline and security-scanning upkeep, documentation cleanups, and a routine supply-chain threat-list refresh; no user-facing behavior changes.

## [0.2.2] — 2026-06-22

### Added

- **Premium lock UI + redemption codes** ([#1724](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1724), [#1730](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1730), [#1737](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1737)) — subscription groundwork: lock badges and gates on premium features, a universal redemption-code system, and server-side per-tier audio/video quality limits applied when joining voice.
- **Public Known Issues list** ([#1736](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1736), [#1769](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1769)) — a pinned, always-current Known Issues tracker in the public feedback repo, linked directly from the in-app feedback dialog so you can check for your issue before filing.
- **"Load latest UI" recovery** ([#1779](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1779)) — a Settings ▸ About button (plus an automatic launch-time retry) that reloads the newest app UI without a restart when a slow network start left you on the built-in fallback.
- **Admin/ops console authentication** ([#1703](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1703)) — operator console sign-in with password plus WebAuthn hardware-key second factor.
- **GIFs load automatically by default** ([#1774](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1774)) — new accounts now start with "Load GIFs from KLIPY automatically" turned on; existing accounts keep their choice.

### Fixed

- **Voice audio crackle and garbling** ([#1765](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1765), [#1777](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1777)) — small calls no longer suffer clipped word starts from unnecessary speaker-limit churn, and silent (DTX) audio frames pass through correctly, fixing garbled audio at the start of speech.
- **Reconnect after server deploys** ([#1770](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1770)) — the app now reliably auto-reconnects after a brief server-side disconnect instead of getting stuck "Reconnecting", and a transient sign-in hiccup no longer wipes a "Remember Me" session.

### Changed

- **Internal tooling, CI, documentation, and dependency maintenance** ([#1731](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1731), [#1733](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1733), [#1762](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1762), [#1771](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1771), [#1772](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1772), [#1775](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1775), [#1776](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1776)) — feedback-triage automation, release-mirror pipeline fixes, security-list refresh, doc cleanups, and opt-in (off by default) audio diagnostics.

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
