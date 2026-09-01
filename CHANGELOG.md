# Changelog

All notable changes to Concord Voice will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

Concord Voice now lets you choose who can see your voice-channel and private-call activity, while keeping that activity current internally. Invite previews stay tied to the invite you opened, and profile-media cleanup now survives storage failures and service restarts — including ordinary clears and account deletion. The visible activity view is not part of this update yet.

### Added

- **You can now choose who sees your voice-channel and private-call activity** ([#3042](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3042)) — Settings ▸ Rich Presence has separate audience and detail controls for server voice and private calls, with a preview of what each choice shares. Turning Rich Presence off hides both categories; people currently in a private call can still see that you are in their call while it is on.

### Changed

- **Desktop utilities are now organized by concern** ([#3045](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3045)) — this internal maintenance change moves existing helpers without changing app behavior, saved data, or security behavior.
- **The desktop now keeps voice-channel and private-call activity current internally** ([#3038](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3038)) — incoming activity is validated by category, snapshots replace stale entries, and disconnects or server switches clear cached activity. This prepares the data path for a later visible activity view; it does not add one yet.
- **Encrypted attachment uploads now negotiate their storage format with the server** ([#3044](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3044)) — when both sides support consistently sized upload pieces, the desktop uses that format; older servers keep receiving the earlier format. This prepares the hosted attachment-storage migration without changing the live storage destination yet.

### Fixed

- **Removing someone from a private conversation no longer causes a database deadlock if the administrator's account is deleted at the same time** ([#3079](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3079)) — account deletion now finishes while removal safely reports changed state, because the two actions lock the affected accounts in one consistent order.
- **Removing someone from a private conversation or deleting its creator no longer leaves stale voice activity after a restart** ([#3052](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3052)) — Concord now records the required activity clear before membership or account deletion removes the information needed to find it.
- **The server can enforce a minimum desktop version before attachment format changes** ([#3053](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3053)) — authenticated REST and WebSocket admission can reject clients below the configured reader floor, while public recovery routes remain available. The declaration is a compatibility signal, not attestation; stored attachment formats remain readable.
- **Encrypted attachments stay on the compatible storage format during rollouts** ([#3051](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3051)) — the server no longer advertises a new format before every supported client can read it, and the desktop rejects capability lists containing unknown formats instead of assuming the remaining entries are safe.
- **Profile-media cleanup now survives storage failures** ([#3043](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3043)) — avatar and banner uploads use immutable generation-suffixed physical keys behind stable canonical profile URLs. Durable pre-upload intents fence ambiguous writes, while unresolved intents and deletion backlog stay bounded by a per-user debt cap. Profile and friend-code avatar reads fail closed and are not cached; a late object-store write can exist briefly, but Concord Voice cannot serve it and keeps deleting it.
- **An invite preview no longer reuses details from a different invite while loading** ([#3037](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3037)) — preview results are now kept with the invite code they describe, so changing codes shows a loading state until the new details arrive.
- **Prepared large encrypted attachments for consistently sized upload pieces** ([#3039](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3039)) — the client and server now understand a new attachment format that reserves room for the header inside the first piece, while retaining the existing write format until compatible server and client versions can be negotiated. Attachments already stored in the older format still open normally.

## [0.2.43] — 2026-08-30

The app can reach the servers again after a certificate change locked every installed copy out. Switching between the app and the web version no longer leaves one of them unable to load your servers. Deleting your account now removes the files it uploaded, not only the record of them.

### Changed

- **Desktop state is now grouped by what it controls** ([#3033](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3033)) — this is an internal maintenance change only; settings, saved data, and app behavior are unchanged.

### Fixed

- **The app can reach the servers again** — every installed copy stopped being able to reach the servers, showing "failed to fetch" when signing in, while the same account worked normally in a web browser on the same machine and on the same network. The app carried its own copy of the server's certificate details and refused to connect to anything that did not match. That certificate is replaced by our hosting provider on its own schedule, and we can neither control nor predict when — so every replacement locked everyone out until a new version of the app was built and installed. That has now happened five times. Nothing could be fixed from our side while it was happening, because the refusal took place on your own machine before any request was sent. The app now checks the certificate the same way your web browser does, which is what the web version has always done. Unexpected certificates are watched for separately, so a genuine problem raises an alert instead of locking everyone out.

### Security

- **Switching between the app and the web version no longer leaves one of them unable to load your servers** ([#3032](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3032)) — if you used the app and then opened the web version at spa.concordvoice.chat, or the other way round, whichever you opened second could fail to load anything at all, while the first one carried on working. When your device asks our servers for something, the reply says which of the two is allowed to use it, and that answer is different for each. The reply did not say it depended on who was asking, so anything holding on to a copy of it — your own browser, or a network between you and us — could keep the answer meant for one and hand it back to the other, which then refused it. The reply now states that it depends on who asked, so a stored copy is only ever reused for the same one. A network that had been holding the wrong answer will stop doing so on its own; nothing needs to be cleared by hand.

- **Deleting your account now removes the files you uploaded, not just the record of them** ([#3019](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3019)) — deleting an account removed every record of the files that account had uploaded, but left the files themselves in storage, and removing those records was the very thing that made the files impossible to find again. Nothing that reclaims storage could see them afterwards, so they stayed indefinitely. Your profile picture and banner are held unencrypted, because the server resizes them for you — so those in particular outlived the deletion meant to remove them. What needs removing is now noted before the account record goes, and deleted once the deletion has definitely gone through. A separate daily sweep reclaims attachments left behind by deletions that already happened. Profile pictures from those earlier deletions cannot be found automatically, and clearing them still needs to be done by hand — that has not happened yet.
- **Changing a server's owner no longer leaves voice activity visible under the old permissions** ([#2991](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2991)) — ownership changes every permission at once, but people already connected were not rechecked when a transfer completed, expired, or was reversed. A transfer could also finish from stale information after the server had changed owner again. Ownership changes now recheck active voice access immediately, and stale transfers are cancelled inside the same locked transaction instead of overwriting newer ownership.

## [0.2.42] — 2026-08-29

Interface improvements reach you again without waiting for an app update. Invite links now arrive, open, and leave the server where you can see it, and the GIF picker's Categories and Recent tabs work for the first time. Someone who has just lost access can no longer be shown that you are online, and the screen-capture protection switch no longer reports itself as on where nothing is enforcing it.

### Fixed

- **Interface improvements reach you again without waiting for an app update** ([#3014](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3014)) — the app can be given interface fixes without you installing a new version, but the compatibility check that decides whether to accept them had become far stricter than it needed to be. Everyone was quietly being served the older copy built into their installed app instead, losing every interface fix since their version was released, with nothing to indicate it had happened. The check now asks what the interface actually needs rather than assuming it needs the newest possible app.
- **"Open in Concord" on an invite link no longer does nothing** ([#2363](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2363)) — the invite was handed to the app and then dropped whenever the app reloaded its own interface, which it does by itself after a network problem. The button appeared to do nothing at all, with no way to tell that anything had been lost. An invite is now held until you have actually been shown it.
- **An invite that arrived while you were offline now appears when you return** ([#2363](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2363)) — invites sent as a direct message were only ever collected while you were looking at your direct messages, so one that arrived while you were away sat unseen until you happened to open that view. It now reaches you without your going looking for it.
- **A server you join from a link now appears in the sidebar straight away** ([#2363](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2363)) — joining through an invite left the server missing from your sidebar until you switched servers or restarted, and its unread badge missing for the same reason. There are two ways to join a server and only one of them was recording it; both now do.
- **The GIF picker's Categories and Recent tabs now show something** ([#2976](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2976)) — both tabs have been empty for everyone since they appeared. Categories were being looked for in the wrong part of the reply and every one was discarded; Recent was never told which account it belonged to, so nothing was ever recorded for it to show. Both now fill.
- **The GIF picker no longer shrinks when a tab has nothing in it** ([#2976](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2976)) — the picker sized itself to whatever it was showing, so moving to an empty tab collapsed the window and moving back grew it again. It now keeps its size.
- **A burst of reconnections no longer holds up messages** ([#2975](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2975)) — working out who is allowed to see your online status takes several trips to the database, and those were made on the same queue that delivers messages and direct messages. When many people reconnected at once — after a restart, or a network problem — that queue waited and everything else waited behind it. The lookups now happen alongside it rather than in front of it.

### Security

- **Someone who has just lost access can no longer be shown that you are online** ([#3010](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3010)) — working out who is allowed to see your online status takes several trips to the database, and that answer could be worked out just before someone's access was taken away and then delivered just after it had gone. Blocking someone, removing a friend, leaving or being removed from a server, having a server deleted around you, and deleting your account were all affected. The answer is now discarded and worked out again whenever access was being changed while it was being calculated.
- **Screen-capture protection no longer reports itself as on where it cannot be enforced** ([#2990](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2990)) — the setting can only be enforced on macOS and Windows. Elsewhere it was accepted, saved, and reported back as active while nothing was actually stopping a capture. It now answers honestly on every platform. A saved preference that cannot be read — a damaged or unreadable settings file, rather than one that was never written — is now treated as off and reported as such, instead of being quietly assumed.

## [0.2.41] — 2026-08-27

A large release. Attachments can finally be as big as your plan has been advertising, you can choose who may send you a friend request, and server roles can be reordered. Several long-standing voice and video faults are fixed — on recent builds of the engine Concord is built on, encrypted audio and video were being handed to the decoder still encrypted, and picture-in-picture windows had never decrypted at all.

### Added

- **Attachments can now actually be as large as your plan says** ([#2157](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2157)) — the size shown for your plan has been right for a while, but sending anything near it failed. Files were encrypted in one piece, and the layer in front of Concord refuses a single upload over 100 MB. Large files are now encrypted and sent in pieces, so the advertised size — up to 256 MiB — is reachable. An upload abandoned partway is now cleaned up rather than leaving the space consumed.
- **You can choose who may send you a friend request** ([#2888](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2888), [#2911](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2911)) — Settings ▸ Privacy now offers *everyone*, *people I share a server with*, or *nobody*, and where you have chosen not to receive them the Send Friend Request button is not offered. Someone who tries anyway is told the same thing, in the same time, as they would be if you had blocked them or if you did not exist — so your setting cannot be worked out by testing against it.
- **Server roles can be reordered** ([#2359](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2359)) — Server Settings ▸ Roles can now be reordered by dragging, by keyboard, or from the toolbar, and the new order is committed with an explicit **Apply Order** rather than saving as you move. It works for anyone allowed to manage roles rather than only the server owner, and you can only move roles below your own highest one.

### Changed

- **Concord now trusts more than one interface-signing key at a time** ([#2958](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2958)) — Concord keeps a verified copy of its own interface, so a network problem drops you back to the last good version instead of an older one built into the app. That copy is only trusted when it was signed with a key baked in at the time your copy of Concord was built, and until now exactly one key was accepted. Changing that key would therefore have silently disabled the cache for everyone who had not yet updated, with no way to tell them otherwise. Concord now accepts a list of keys, so the outgoing and incoming key both work while a changeover is in progress and nobody loses the cache in between.

### Fixed

- **Voice and video are no longer garbled on recent builds** ([#2866](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2866)) — a change in the browser engine Concord is built on stopped decryption being applied when it was set up a moment after a stream had started. Encrypted audio and video were handed to the decoder still encrypted: noise instead of speech, a black picture instead of video. Decryption is now set up as the stream is created, and where it genuinely cannot be applied the stream is refused rather than played back as noise.
- **Picture-in-picture windows now decrypt what they show** ([#2870](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2870)) — a picture-in-picture window never applied decryption at all, so microphone, camera and screen share were all fed to it still encrypted — garbled audio and a black picture, every time, for as long as the window has existed. It now decrypts, and closes the stream rather than showing noise if it cannot.
- **The connection to Concord no longer drops every few minutes** ([#2873](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2873)) — connections through the hosted service were closing abnormally every few minutes and reconnecting within half a second. Brief, but voice could be torn down along with it. The connection is now kept alive properly, and a voice session is no longer ended by a disconnection that recovers inside its grace period.
- **One cause of a voice server disconnecting everyone at once is fixed** ([#2868](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2868)) — when a voice server's internal queue overflowed it disconnected every client on that machine rather than the one session responsible. That case no longer does. Worth stating plainly: this is one cause removed, not the behaviour eliminated — the same over-broad disconnect is still reachable by other routes, and those are still being worked through.
- **Mentions in direct messages show names again** ([#2368](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2368)) — a mention inside a direct message showed a raw identifier instead of the person's name. Names are now resolved from the people in that conversation, and the raw form appears only while the conversation is still loading.
- **A restart no longer leaves your activity showing after you have left** ([#2910](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2910)) — if the server restarted between your leaving a voice channel or a call and that being sent out, nothing durable recorded that the clear was owed, so others kept seeing you there until a ninety-second timeout ran out. That clear now survives a restart.
- **A storage fault no longer reveals which friend codes exist** ([#2860](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2860)) — the page behind a friend code answers every invalid code identically on purpose, so guessing at codes tells you nothing. While the avatar store was unwell it answered differently for a real code than an invented one — enough to sort guesses into hits and misses. It now answers the same way in both cases.

### Security

- **An opt-in setting can stop Concord's windows being captured** ([#2977](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2977)) — on macOS and Windows you can now ask the operating system to leave Concord's main and picture-in-picture windows out of screen recordings and screenshots. It is off unless you turn it on, and it is not offered on Linux, where it cannot be enforced. Packaged builds also switch off several Electron capabilities that are only useful for inspecting a running application.

## [0.2.40] — 2026-08-20

Friend codes now have a web page of their own. Opening someone's friend link in a browser shows who it belongs to and offers to open Concord, and the page looks the same whether the code is live or not, so nobody can use it to work out which codes exist.

### Added

- **Friend code links now open a real page, and open the app** ([#945](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/945)) — a friend link shared outside Concord used to lead nowhere useful. It now opens a page showing the username, display name and avatar of whoever the code belongs to, with a button that opens Concord straight to the request. Codes that have expired, been revoked, or been used up show a neutral "code unavailable" page — deliberately the same size and shape as a live one, so someone guessing codes cannot tell from the page which guesses landed.

### Fixed

- **Clicking several friend or invite links quickly no longer loses the ones in the middle** ([#945](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/945)) — links arriving close together were collapsed to the newest, so clicking three in a row opened only the last. All of them now open, in the order you clicked, one per second. A repeat of the link already on screen is still ignored, since re-opening it would change nothing.
- **A friend link clicked before signing in no longer follows you into someone else's account** ([#945](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/945)) — a link clicked while signed out was held and opened after signing in, which is intended, but it was never cleared when you signed out. On a shared computer the next person to sign in saw it. Signing out now discards anything held.
- **A brief server problem no longer reports a working friend code as dead** ([#945](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/945)) — if the server was briefly busy or unwell, the page said the code was no longer valid and that answer was remembered for a minute, outliving the problem. The page now distinguishes "this code is not valid" from "we could not check right now", and recovers within seconds.
- **Revoking a friend code now tells you when it fails** ([#945](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/945)) — if revoking failed — offline, signed out, or a server error — nothing was shown and the code stayed listed, so you could believe a still-live code had been withdrawn. Revoking is the only way to take a friend code's public page offline, so the failure is now reported.

### Fixed

- **Leaving, being kicked from, or being banned from a server now stops your activity showing there immediately** ([#2447](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2447)) — until now, someone who left or was removed from a server could keep seeing what its members were doing in voice for up to a minute and a half, and a member who had just joined saw nothing until the next thing happened. Both are fixed: the people who can see you are worked out and updated at the moment membership changes. Being kicked or banned now signs you out on every device you have open rather than refreshing what you can see, so nothing stale is left behind. Someone who can still see you another way — a second shared server, or through friends — is unaffected and is not cleared by mistake.
- **Deleting a server, or deleting your account, no longer leaves your activity visible to others** ([#2447](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2447)) — the information needed to work out who could see you used to be erased before it could be used, so people kept a stale view. That information is now captured first and cleared afterwards. Deleting your account also reaches people connected to other servers in the cluster, which it previously did not. One limit worth stating: when a **server** is deleted, a member who is offline at that moment may still see a stale custom status from it until they next reconnect — live voice activity clears within about ninety seconds either way.

### Changed

- **A friend or privacy change can briefly delay saving your custom status** ([#2823](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2823)) — accepting or removing a friend, blocking someone, redeeming a friend code, or changing who can see you through friends-of-friends now updates your custom status for everyone who can see it, and does so durably rather than only for people currently connected. While one of those is being applied, your own **Settings ▸ Presence** save may return "service unavailable" for up to 30 seconds, with the wait shown in a `Retry-After` header. That is expected rather than a fault: both writes touch the same presence state, and they are serialized so neither can publish an audience that is already out of date. Retrying after the indicated delay succeeds.

## [0.2.39] — 2026-07-31

Pointing Concord Voice at a self-hosted server is now checked at the moment it connects, not only when you type the address. A server on your own network or machine is reachable only after you approve it in a confirmation the web layer cannot draw or click.

### Fixed

- **A compromised web layer can no longer make the app read your private network** ([#2668](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2668)) — when you pointed Concord Voice at a self-hosted server, the app checked that the address looked well-formed but never checked where it actually led. That let a hostile page inside the app reach devices on your own network, or your machine itself, and read what came back. Addresses are now checked at the moment the connection is made, addresses that could never host a server are refused outright, and a server on your own network or machine is reachable only after you have approved that server in a confirmation the web layer cannot draw or click. Approving a server is also now a separate, deliberate step rather than something a successful connection did on its own.

## [0.2.38] — 2026-07-30

Encrypted channel keys now reach every member of a busy server without stalling, retrying forever, or handing out a key that was already revoked. The DM and server sidebars share one adaptive layout, so both behave the same way when you narrow the window. Several sign-in paths were tightened so an abandoned or superseded attempt cannot disturb the session that replaced it.

### Changed

- **The DM and server sidebars now share one adaptive layout** ([#2510](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2510)) — both sidebars resize by the same rules, so a narrow window behaves the same whether you are in a server or in direct messages.
- **Updated the voice server and interface libraries** ([#2513](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2513), [#2515](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2515), [#2517](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2517)) — the media server moves to mediasoup 3.22.0 and the interface moves to the current React release.

### Fixed

- **Encrypted channel keys are delivered in batches** ([#2503](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2503)) — a channel with many members no longer sends one key request per member at once. Distribution is grouped, so a large server finishes key delivery instead of overwhelming the server and stalling.
- **Key distribution can be cancelled while it waits to retry** ([#2541](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2541)) — when the server asks the app to slow down, the app waits before trying again. Leaving the channel during that wait now cancels the work instead of letting it resume against a channel you left.
- **Key recovery resumes after a repeated rotation** ([#2539](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2539)) — two rotation batches arriving for the same channel could leave a member waiting for a key that was never re-requested. Recovery now wakes on the second batch.

### Security

- **Signing up with a password after backing out of a social sign-in no longer costs you your encryption keys** ([#2655](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2655)) — if you started signing in with Google or Apple and then changed your mind, the abandoned attempt kept a hold on the app's secure-key slot. A password registration that followed silently failed to save its encryption keys to your keychain, so those keys did not survive a restart until you logged in again. The abandoned attempt is now released before registration begins.
- **Two-step verification codes are compared in constant time** ([#2654](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2654)) — the comparison no longer stops early on the first wrong character, so the time it takes to reject a code reveals nothing about how much of it was correct.
- **A key from a revoked epoch is refused** ([#2534](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2534)) — the server rejects distribution into a channel epoch that has already been revoked, so a removed member cannot be handed a key that a rotation was meant to take away.
- **Manual channel rotation is fenced to the current credentials** ([#2540](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2540)) — a rotation started before a password change or key reset can no longer commit afterward.
- **A channel's active key epoch survives concurrent writes** ([#2542](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2542)) — the recorded epoch is read from the durable ledger, so two overlapping rotations cannot leave the channel pointing at the wrong one.
- **Signing out other sessions no longer signs out the one you are using** ([#2501](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2501)) — revoking all sessions preserves the session that issued the request.
- **Rich Presence suppression fails closed** ([#2522](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2522)) — if the server cannot confirm that your activity should be hidden, it hides it rather than publishing it.
- **Returning from invisible does not replay a stale suppression** ([#2502](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2502)) — a suppression queued while you were hidden is discarded when you come back online, so your activity is not blanked immediately after you choose to show it.
- **Resolved static-analysis findings in authentication handling** ([#2525](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2525)) — no user-visible behavior changed.

## [0.2.37] — 2026-07-27

Sign-in and session handling were hardened across the board: a stolen or superseded token can no longer outlive the reset that was meant to end it, and an account that is disabled while connected is disconnected instead of lingering. Recovery keys now use the stronger elliptic curve the rest of Concord Voice already required.

### Fixed

- **Desktop updates no longer risk a crash or missing preload files** ([#2492](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2492)) — updated Electron to 43.1.1, which fixes failures when an installed app archive is replaced while Concord Voice is still running and includes Chromium 150.0.7871.114.
- **Encrypted key requests no longer flood the server** ([#2482](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2482)) — the number of key requests the app sends at once is bounded, so rejoining a large server does not produce a burst the server must shed.
- **Bulk message removal is more reliable** ([#2483](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2483)) — purging a member's messages records its progress, so an interrupted purge can be identified and completed instead of stopping silently partway.

### Security

- **Linking a social account is bound to the password you just verified** ([#2458](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2458)) — if your credentials change between entering your password and completing the link, the link is refused instead of completing against the old proof.
- **Token-theft revocation and session creation cannot interleave** ([#2460](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2460), [#2457](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2457)) — when a reused refresh token triggers a revocation, a sign-in running at the same moment can no longer slip a new session past the sweep.
- **A disabled account is disconnected from an open connection** ([#2480](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2480)) — the live connection checks account state rather than trusting the token it was opened with, so disabling an account takes effect immediately instead of at the next reconnect.
- **Recovery keys use P-384** ([#2355](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2355)) — account-recovery key agreement now enforces the same curve Concord Voice requires everywhere else. Weaker curves are rejected.
- **Going invisible or offline clears activity that was already published** ([#2461](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2461)) — voice and call activity is suppressed on the transition itself, so nothing you were doing stays visible after you hide.

## [0.2.36] — 2026-07-26

Desktop actions that touch your camera, clipboard, updates, or permissions now verify the request came from Concord Voice itself before acting.

### Security

- **Desktop actions now reject untrusted web content** ([#2351](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2351)) — screen capture, clipboard, update, permission, lifecycle, and developer controls now act only on calls from a permitted Concord Voice frame. Floating call windows remain usable while the app switches between remote and bundled interface sources.

## [0.2.35] — 2026-07-25

Windows updates install when you click Restart. The previous installer deleted its own files partway through and stopped, which is why updates appeared to do nothing.

### Fixed

- **Windows updates now install when you click Restart** ([#2402](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2402)) — after downloading an update, clicking Restart closed Concord Voice and then nothing happened; a window flashed briefly and disappeared. Running the downloaded installer by hand did the same thing, and the only way through was to move that file somewhere else, like your Downloads folder, and run it from there. Updates now install normally from the Restart button, with no manual step. Leftover files from the old installer are tidied up on a later launch, and your downloaded updates and your installed app are never touched by that cleanup.

## [0.2.34] — 2026-07-25

Version 0.2.33 never reached you — it was bumped but never released, so everything it carried ships here. On top of that: signing in with Google or Apple is bound to the account state at the moment it completes, and the "can't reach Concord servers" warning clears itself once the connection is up.

### Fixed

- **The "can't reach Concord servers" warning at startup now clears itself** ([#2401](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2401)) — this warning could appear when the app opened and then stay on screen even though you were connected and your messages were loading, only going away if you manually refreshed the window. It now disappears on its own the moment the app reaches the servers, and it no longer appears at all when the connection was already up. Startup warnings that are not about reachability — a configuration the app declined to use, or an app version too old for the current interface — still appear as before, and now say which of those actually happened instead of blaming the connection.

### Security

- **Social sign-in is bound to your current account state** ([#2453](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2453)) — the session is created while the account record is held, so a password reset or key recovery running at the same moment cannot leave a sign-in behind that the reset was meant to cancel.
- **Updated the desktop router to close a published advisory** ([#2442](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2442)) — resolves GHSA-QWWW-VCR4-C8H2.
- **Encryption key self-healing checks the recipient's current key** ([#2440](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2440)) — when the app re-sends a channel key to someone who missed it, it names the key version it wrapped for. The server refuses the delivery if that person has since reset their keys, so a key is never wrapped to an identity that no longer exists.

## [0.2.33] — 2026-07-24

This release was bumped but never published; its contents reached you in 0.2.34. It is the largest security pass of the beta so far. Sessions, encryption keys, and password changes are now tied to the exact credentials that authorized them, so a stale sign-in, a slow reply, or an interrupted password change can no longer disturb the session that replaced it.

### Fixed

- **Key recovery stays reachable after a password sign-in** ([#2435](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2435)) — the app holds the main route until encryption is ready, so you are not dropped into the app in a state where recovery cannot be started.
- **Custom Status visibility settles consistently** ([#2419](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2419)) — the record of who may see your status is reconciled durably, so a failed update is repaired rather than left half-applied.
- **Rich Presence cleanup resumes after an interruption** ([#2408](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2408)) — if hiding your activity fails partway, the evidence of what still needs hiding is kept and retried instead of being derived from state that has already moved on.
- **Private calls appear only once both sides are admitted** ([#2407](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2407)) — a call that is still being authorized no longer shows up as a live call or leaves a phantom entry behind if it is abandoned.
- **Hidden presence stays hidden across servers** ([#2404](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2404), [#2405](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2405), [#2232](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2232)) — presence state is synchronized rather than recomputed per connection, malformed stored activity is repaired instead of trusted, and a superseded presence generation is verified before it is allowed to write.
- **Reduced complexity in sign-in and voice lifecycle code** ([#2421](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2421)) — no behavior change; the paths involved in the fixes above were made easier to reason about.

### Security

- **Two-step verification during a social sign-in completes inside the app's protected process** ([#2436](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2436)) — your credentials stay tied to that exact sign-in, so a stale or superseded attempt cannot overwrite a newer session. The main window also never unlocks before encryption is ready for the account that just signed in.
- **Encryption keys are committed by the sign-in that created them** ([#2434](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2434)) — a sign-in that has been replaced can no longer clear or overwrite the keys belonging to the one that replaced it.
- **A password change that succeeded is never reported as a failure** ([#2431](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2431)) — if the server confirms the change but the reply cannot be read, the app signs you out and asks you to sign in again rather than continuing with keys that no longer match your password.
- **Key delivery checks the recipient against a concurrent key reset** ([#2430](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2430)) — a channel key is not wrapped to someone whose keys were reset while the delivery was in flight; the delivery is skipped and re-queued against their new key.
- **A slow rejected request cannot end a rotated session** ([#2432](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2432)) — a rejection that arrives after your session has already been refreshed is ignored instead of signing you out.
- **Grace-period session refresh follows an exact lineage** ([#2428](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2428)) — when two devices refresh at nearly the same moment, the replacement session is matched to the exact token it replaced rather than guessed from timing.
- **Every session is bound to the credentials that authorized it** ([#2427](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2427), [#2397](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2397)) — a password change or key recovery advances a per-account marker, and any sign-in still holding the previous marker is refused. This closes the window where a session authorized just before a reset could be created just after it.
- **Password changes rotate every encrypted setting together** ([#2395](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2395)) — saved GIFs, interface preferences, friend organization, and Custom Status visibility are re-encrypted in one transaction. Previously a failure partway could leave some of them locked to your old password.

## [0.2.32] — 2026-07-22

Sign-in and session refresh stay with the account and server you chose, even when two sign-ins overlap or you switch between hosted servers.

### Fixed

- **Sign-in and refresh stay with the right account and server** ([#2374](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2374)) — session rotation, simultaneous sign-ins, and switching between hosted servers can no longer mix credentials or continue a stale sign-in against the newly selected server, and a superseded sign-in no longer leaves its encryption keys resident in memory.

### Changed

- **Updated the voice server and build libraries** ([#2379](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2379), [#2384](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2384), [#2380](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2380), [#2389](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2389)) — mediasoup, Vite, the icon set, and the media server's request parser move to their current releases.

## [0.2.31] — 2026-07-22

Groundwork for showing what your friends are doing. The server can now publish voice and call activity, and the desktop app validates it without displaying it yet.

### Changed

- **Privacy-safe voice activity groundwork** ([#2391](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2391)) — the server can now publish authorized Server Voice and Private Call activity, and the desktop safely validates the new wire format without displaying it yet. A later client update will add the visible experience.

## [0.2.30] — 2026-07-18

Reconnecting after a network drop now restores what changed while you were away, instead of leaving stale servers and missing messages on screen. Moderators can remove a member's messages when kicking or banning them, and Apple sign-in works again on desktop.

### Added

- **Removing a member can remove their messages** ([#2342](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2342)) — kicking or banning a member can also purge the messages they posted in that server.
- **Account activity metrics and an Admin workspace** ([#2338](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2338)) — operators can see aggregate account activity. The figures are counts only; they carry no user, server, or channel identity.
- **Safe rotation of the two-step verification encryption key** ([#2345](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2345)) — operators can rotate the key that protects stored two-step secrets. Each stored secret records the key version that sealed it, so old and new keys coexist during a rotation.

### Fixed

- **Reconnecting restores what changed while you were disconnected** ([#2327](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2327), [#2358](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2358)) — a brief network drop used to leave the app showing servers you had been removed from and missing messages sent during the outage, until you reloaded. Reconnecting now refreshes your memberships and back-fills the open conversation. Your unsent drafts and queued messages are kept, which the previous behavior discarded.
- **Apple sign-in completes on desktop** ([#2323](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2323)) — the sign-in returns through the hosted bridge rather than a local address, which the previous flow could not reach.

### Security

- **A password change that loses your encryption keys fails closed** ([#2357](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2357)) — the app signs you out and asks you to sign in again rather than continuing in a state where your keys no longer match your password.
- **Encryption keys are not cleared by a sign-in that has ended** ([#2337](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2337)) — a sign-in torn down mid-setup can no longer clear the keys belonging to the sign-in that replaced it.
- **Security headers are sent once, from one place** ([#2328](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2328)) — duplicate and conflicting headers are removed, and the transport-security policy is stated once.

## [0.2.29] — 2026-07-15

Moderators can now remove a member's messages in bulk, across a channel, a server, or a conversation. Voice gets more honest about video: the codec shown in Settings is the one actually in use, and H.264 calls are encrypted per access unit rather than whole-frame. Operators get an Admin console with aggregate figures that carry no user identity.

### Added

- **Bulk message removal** ([#2290](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2290)) — a moderator can delete a member's messages across a channel, a whole server, or a conversation in one action. The removal is recorded as counts and context only; message text is never written to the audit record.
- **The Admin Portal console** ([#2268](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2268)) — operators get a web console for instance health and aggregate metrics, behind its own sign-in.
- **Rich Presence category settings persist** ([#2275](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2275)) — your per-category choices for what activity is shared are stored, so they survive a restart.

### Fixed

- **The codec shown in Settings is the one in use** ([#2242](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2242)) — the video settings screen previously showed the codec the app intended to use. It now separates what will be tried from what is actually running, read from the live connection, so hardware encoding is never claimed when software encoding is doing the work.
- **H.264 video is encrypted per access unit** ([#2298](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2298)) — H.264 calls use a format-aware encryption boundary instead of whole-frame encryption, which lets the video server route frames without ever seeing their contents. HDR target selection was corrected at the same time.
- **Admin metrics read correctly** ([#2286](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2286), [#2287](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2287), [#2281](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2281)) — hourly averages stay inside their bounds, chart values are formatted and labelled, and host telemetry is restored in production.

### Security

- **Desktop release signing is isolated from pull requests** ([#2296](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2296)) — pull-request packages now run unsigned without signing secrets or OIDC, while signed releases rebuild protected `main` with environment-scoped credentials and same-run artifacts.
- **Voice activity is authorized before it is published** ([#2282](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2282)) — the server checks that a viewer is allowed to see your Server Voice or Private Call activity, and publishes the minimum needed to render it.

## [0.2.28] — 2026-07-14

Direct-message call previews now say what actually happened, and Custom Status counts characters the way you would.

### Fixed

- **DM call previews describe the call** ([#2245](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2245)) — a call entry in your conversation list now reflects its outcome instead of showing one generic line for every case.
- **Custom Status counts characters, not bytes** ([#2250](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2250)) — the length limit counts Unicode code points, so accented letters, non-Latin scripts, and emoji no longer consume several characters each.

### Changed

- **Desktop build and test tooling refreshed** ([#2265](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2265), [#2264](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2264), [#2261](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2261)) — updated tsx, Vitest coverage tooling, ESLint React tooling, and TypeScript ESLint behind the desktop build and test pipeline.

## [0.2.27] — 2026-07-14

Activity History arrives: an opt-in, self-only record of your own voice and call intervals, off until you turn it on and deleted on your schedule. Video codec selection was also reworked so a call that drops to software encoding re-selects instead of staying there.

### Added

- **Activity History — opt-in and self-only** ([#1235](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/1235)) — you can record your own voice and call intervals and review them in Settings. It is off until you consent, the record is visible only to you, and it is pruned on the retention window you choose. Consent is versioned: if the terms change, you are asked again rather than opted in silently. Nothing is recorded while it is off.
- **A read-only admin metrics API** ([#2228](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2228), [#2218](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2218)) — operators can read aggregate instance metrics through a restricted role. The figures are fixed numeric counts with no user, server, or channel dimension.

### Fixed

- **Video re-selects its codec mid-call** ([#2187](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2187)) — when a call falls back to software encoding, the app now re-selects toward a hardware-capable codec instead of staying on the slower path for the rest of the call. Re-selection is serialized so two triggers cannot fight, and cannot oscillate between two software codecs.
- **Chat stays where you left it** ([#2227](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2227)) — a late layout change no longer scrolls you away from the newest message.
- **Edited encrypted messages display correctly** ([#2217](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2217)) — an edit is decrypted before it is stored, so an edited message no longer shows as unreadable.
- **Dialog placement corrected** ([#2225](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2225)) — the outgoing-call and What's-new dialogs no longer appear behind other windows.

### Security

- **Safer desktop update checks** ([#2247](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2247)) — updated the parser used by Concord Voice's desktop updater to prevent specially crafted recovery-feed YAML from consuming excessive CPU during update checks. Routine security maintenance; no action needed.

## [0.2.26] — 2026-07-12

Screen sharing was rebuilt around what viewers can actually see. Every participant can share at once, each stream carries its own volume and mute, and the server sends each viewer only the resolution their window needs. Video codec selection now learns from the live call whether hardware encoding is really being used.

### Added
- **Everyone can share their screen at once** ([#2160](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2160), [#2185](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2185)) — concurrent screenshare limits were raised substantially, and a shared screen is sent at several qualities so each viewer receives the one that fits their window instead of everyone paying for the largest.
- **Per-stream screenshare audio** ([#2169](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2169)) — each shared screen has its own volume slider and mute. Muting one stops the server sending you that audio at all, rather than silencing it locally.
- **A voice tile view** ([#2177](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2177)) — participants fill the available space, and the view switch moved somewhere you can find it.
- **Custom Status recipient exceptions** ([#2191](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2191)) — you can hide your Custom Status from specific people. The exception list is encrypted with your password-derived key, so the server enforces it without being able to read it.

### Changed
- **Subscriptions expire on schedule** ([#2168](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2168)) — when a plan reaches the end of its period, entitlements return to the free tier automatically instead of waiting for the next sign-in.

### Fixed
- **Hardware video encoding is detected from the live call** ([#2184](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2184), [#2189](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2189)) — the app probes what your machine can encode and then confirms it against the running call, so a codec that claims hardware support but silently falls back is demoted for the session.
- **Voice survives a brief server blip on join** ([#2181](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2181)) — joining a call during a short interruption now retries instead of failing outright.
- **Picture-in-picture windows close promptly when a call ends** ([#2215](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2215)) — leaving voice now releases each floating window's media immediately instead of leaving an always-on-top window visible while cleanup requests time out.
- **System audio is captured only for whole-screen shares** ([#2165](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2165)) — sharing a single window no longer captures audio from everything else.
- **The focused screen stays focused** ([#2156](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2156)) — tuning into a second screen no longer displaces the one you were already watching.
- **Update settings are quiet at rest** ([#2183](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2183)) — interface and app update state are reported separately instead of one status standing in for both.
- **Renamed Settings ▸ Sounds and Notifications to Notifications** ([#2188](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2188)).
- **Screen quality demand no longer flaps for AV1 and VP9 shares** ([#2212](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2212)) — layer preferences for those codecs apply directly instead of feeding the decision about whether to publish several qualities.
- **Follow-up fixes from the desktop notification work** ([#2167](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2167)) — remaining issues found while auditing the notification changes.

### Security
- **Voice publishing is checked by the media server** ([#2140](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2140)) — permission to speak or share is enforced where the media is actually accepted, not only in the interface.
- **An empty server-supplied voice identity fails closed** ([#2152](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2152)) — following the change in 0.2.25, a blank authoritative name is used as-is rather than falling back to the name the client supplied.
- **Channel video limits follow the server's plan, not the owner's** ([#2175](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2175)) — per-room camera and screenshare limits are resolved from the server's own subscription, so a premium member or owner on a free server cannot raise the limits for that room.
- **Screenshare resolution and frame rate follow your plan** ([#2172](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2172)) — the limit is a combined pixel-rate, so 1080p30 and 720p60 both work on the free tier while 1080p60 does not.
- **Shutdown no longer strands connections** ([#2203](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2203), [#2149](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2149), [#2214](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2214)) — the server drains in-flight requests before closing live connections, and voice transports are released by their owner.

## [0.2.25] — 2026-07-09

A full security audit of channel and message permissions landed: what you can see is now checked on every path that can reveal it, not only on the one that lists it. The message composer also grew up — emoji autocomplete, keyboard shortcuts for the pickers, and no more layout jump on first open.

### Added

- **Emoji autocomplete in the composer** ([#2081](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2081)) — type a `:shortcode:` and pick from suggestions inline.
- **Keyboard shortcuts for the pickers** ([#2079](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2079)) — Ctrl+E opens emoji, Ctrl+G opens GIFs, from the composer.
- **Tune in and out of individual screens** ([#2126](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2126)) — you choose which shared screens to watch, globally or one at a time.
- **See exactly what a bug report sends** ([#2086](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2086), [#2082](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2082)) — a preview shows the diagnostic log attached to a report before you send it. Identifiers are replaced with per-report placeholders, and tokens, addresses, and file paths are removed.

### Fixed

- **The composer no longer jumps when a picker first opens** ([#2075](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2075), [#2085](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2085)) — picker code is loaded ahead of the first open, so the composer stops expanding and snapping back.
- **Emoji-only messages render large again** ([#2073](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2073)) — a message that is only `:shortcode:` emoji scales up, matching the behavior for literal emoji.
- **Privacy and Security settings no longer sign you out** ([#2067](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2067)) — a background settings sync that failed used to be treated as an expired session.
- **Dialogs trap focus correctly** ([#2089](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2089)) — modals announce themselves to screen readers, keep keyboard focus inside while open, and return focus where it came from.

### Security

- **Channel visibility is enforced everywhere it can leak** ([#2134](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2134), [#2136](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2136), [#2138](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2138), [#2132](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2132), [#2130](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2130)) — channel metadata, unread counts, attachments, encryption keys, live update broadcasts, and voice participant lists are all gated on permission to view the channel, rather than on server membership alone. Previously a member of the server could learn about channels they could not open.
- **Removed members are evicted from live connections** ([#2133](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2133), [#2135](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2135)) — kicking or banning a member closes their live subscription immediately, and voice counts are scoped to servers the recipient is actually in.
- **Group conversation creation respects your privacy settings** ([#2127](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2127)) — you cannot be added to a group conversation by someone your settings do not permit to message you.
- **Channels cannot be bound across servers** ([#2131](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2131)) — a channel category must belong to the same server as its channel, enforced by the database rather than by the caller.
- **Sign-in state is consumed atomically** ([#2129](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2129), [#2139](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2139)) — single sign-on state and one-time tokens are read and deleted in one step so they cannot be replayed, and device attestation tokens are bound to the account that minted them.
- **Voice display identity comes from the server** ([#2143](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2143)) — the name and avatar other participants see are resolved from your authenticated account rather than sent by your app, so a modified client cannot present itself as someone else in a call.
- **Admin enrollment links no longer carry their token in the address** ([#2137](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2137)) — the enrollment token is kept out of the URL, where it would be recorded in history and server logs.
- **A wildcard origin is refused in production** ([#2128](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2128)) — the server refuses to start with a permissive cross-origin setting, which would have allowed a hostile page to make credentialed requests.
- **Archive handling hardened against malicious files** ([#2142](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2142)) — updated the tar library used by our desktop build tooling and the voice server's installer so maliciously crafted archives (compression bombs and malformed headers) can no longer hang or crash those steps. Routine hygiene — no user action needed.
- **Verification failures respond consistently** ([#2083](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2083)) — running out of verification attempts returns the same shape as any other failure, so the response reveals nothing extra.

## [0.2.24] — 2026-07-05

A batch of things that were quietly broken: username search, the friend-request sound, password-reset email, and the verification-code window that was too short to type in. Voice also picked up a visual layer that shows who is speaking and that the call is encrypted.

### Added

- **A voice stage that shows who is talking** ([#2059](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2059), [#2062](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2062)) — the active speaker takes the foreground, an encryption ring marks the call as end-to-end encrypted, and a backdrop appears before you join. Messages that fail to decrypt are now visibly distinct rather than blank.
- **Show your password while typing it** ([#2061](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2061)) — the sign-in screen has a reveal toggle.

### Fixed

- **A clearer signal when your encryption keys can't be saved** ([#2068](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2068)) — if your device keychain is locked or full when Concord saves your encryption keys for next launch, the app now notices instead of failing silently. Your current session keeps working either way; if saving didn't succeed, signing in again on the next launch restores it.
- **Searching for a user by name works again** ([#2057](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2057)).
- **Friend requests make a sound again** ([#2054](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2054)).
- **Password-reset email is delivered** ([#2058](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2058)) — the reset message failed to send; it now arrives.
- **There is time to enter the email verification code** ([#2055](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2055)) — the window was short enough that the code often expired while you were reading it.
- **One-to-one calls show a single bottom bar** ([#2056](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2056)) — the duplicate bar is gone.
- **Settings and help overlays close** ([#2064](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2064)) — both can be dismissed without reaching for the keyboard.
- **A GIF that fails to load no longer signs you out** ([#2051](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2051)) — a rejection from the third-party GIF service is no longer read as your session expiring.

### Security

- **The test environment setting is refused in production** ([#2060](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2060)) — the server will not start in production with test-mode configuration, which relaxes several checks.

## [0.2.23] — 2026-07-04

Your subscription is visible and manageable in the app, and call quality, upload limits, and server capabilities follow the plan you are on. When bandwidth gets tight, Concord Voice sheds webcam before screenshare before audio, so voice stays clear.

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

Update Concord Voice and the next launch tells you what changed. This file became the canonical public record at the same time, and the release pipeline now refuses to cut a version without an entry for it.

### Added

- **See what changed, right after you update** ([#2034](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2034)) — update Concord Voice, and the next launch shows a "What's new" dialog covering every version since the one you had. It shows once, works offline, and never slows startup. Read it or dismiss it — your call.
- **A changelog you can hold us to** ([#2034](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2034)) — this file is now the canonical public record of every change we ship. Every release since Beta is documented below, and CI refuses to cut a new version without its entry. No entry, no release.

## [0.2.21] — 2026-07-02

Windows verifies an update's signature before installing it, and public releases carry build provenance you can check yourself. A batch of chat fixes came with it: pinning works again, scroll stays where you left it, and previews say what they mean.

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

Self-hosted instances get the full feature set with no artificial limits. The licence, privacy policy, and terms are one click away in About.

### Added

- **Self-host it, get everything** ([#1985](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1985)) — run Concord Voice on your own hardware and every premium entitlement is unlocked out of the box. No subscription required. That is the self-hosting deal: your server, your rules, all of it.
- **Legal documents, one click away** ([#1987](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1987)) — the license, terms, and other notices are readable directly from Settings ▸ About.

## [0.2.19] — 2026-06-30

You can sign in to a Concord Voice server you run yourself. Clients that lost track of the hosted interface can now find their way back without reinstalling.

### Added

- **Sign in to your own server** ([#1982](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1982)) — the first slice of self-hosted support: desktop login can now route to an operator-run Concord Voice server instead of the managed service.

### Fixed

- **Stranded clients can find their way home** ([#1984](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1984)) — the app now checks the public release feed for updates, so a client stuck on an old version can always recover and get current again.

### Changed

- **Behind-the-scenes deployment and tooling upkeep.**

## [0.2.18] — 2026-06-29

Certificate pinning is back on for connections to Concord Voice servers.

### Security

- **Certificate pinning restored** ([#1983](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1983)) — our certificate pinning for Cloudflare-fronted connections had lapsed. This release brings it back: connections to Concord Voice services are once again verified against the exact certificates we expect. We are telling you it lapsed because you deserve to know — that is the point of this file.

## [0.2.17] — 2026-06-29

React to direct messages with emoji. Connecting at launch is more reliable, and rotating a certificate no longer locks you out of your own server.

### Added

- **React to direct messages** ([#1976](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1976)) — DMs now take emoji reactions, the same way server channels already do.

### Changed

- **Behind-the-scenes tooling, CI, and dependency upkeep** ([#1979](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1979)).

### Fixed

- **You connect reliably, right from launch** ([#1978](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1978)) — we removed a race in the WebSocket authentication handshake that could leave the client sitting disconnected just after startup.
- **Certificate rotation no longer locks you out** ([#1980](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1980)) — clients enforcing the old production API certificate pin failed to connect after we rotated the certificate. The client now trusts the rotated pin.

## [0.2.16] — 2026-06-29

Choose how much a notification reveals on screen. Every colour scheme is now free for everyone, and opening direct messages no longer interrupts your call.

### Added

- **Choose what your notifications reveal** ([#1970](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1970)) — a new setting controls how much message content appears in desktop notifications, so nothing private shows up on a shared or public screen unless you want it to.
- **Custom themes are now free** ([#1974](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1974)) — build your own theme, no subscription required.

### Fixed

- **Your call keeps playing when you open DMs** ([#1972](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1972)) — the voice audio pipeline stays mounted while you browse direct messages, so call audio no longer drops mid-navigation.
- **macOS relaunches after installing an update** ([#1973](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1973)) — "Restart to update" on macOS quit the app and then didn't bring it back. The update-install restart path now reopens Concord Voice for you.

### Changed

- **Behind-the-scenes tooling, CI, and supply-chain upkeep** ([#1968](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1968), [#1969](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1969)) — we retired a legacy automation credential and automated the guard on our supply-chain indicator-list refresh. Nothing changes for you; the pipeline gets safer.

## [0.2.15] — 2026-06-29

Moderators can time a member out instead of removing them. Passkey registration is stricter, and erasing your account now revokes the session that asked for it.

### Added

- **Member timeout moderation** ([#1967](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1967)) — moderators can put a member in timeout for a set duration. Participation is restricted until the clock runs out — no permanent ban required for a temporary problem.
- **macOS Applications-folder move prompt** ([#1966](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1966)) — the first time you launch Concord Voice from outside /Applications, it offers to move itself there. One click gets you a proper install and updates that land reliably.

### Changed

- **Behind-the-scenes tooling, CI, and deployment upkeep.**

### Security

- **Stricter passkey registration requirements** ([#1963](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1963)) — WebAuthn registration now demands passkey-grade platform authenticator options. Every new passkey you create meets the security bar we set — no silent downgrades.
- **Access token revoked on account erasure** ([#1965](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1965)) — erase your account and your current access token dies with it. No authenticated session outlives the deletion.

## [0.2.14] — 2026-06-28

A channel can set one audio standard for everyone in it, so voice quality no longer depends on each person's plan. Usernames became case-insensitive everywhere, and self-hosted certificate failures are reported instead of swallowed.

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

Invite bubbles name the person who actually invited you. Linux updates are signature-verified before they install, and the signed offline interface cache went live.

### Fixed

- **Invite bubbles now name the person who actually invited you** ([#1909](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1909)) — Send-to-a-Friend invite messages in chat were attributed to the wrong user. Now the sender shown is the sender who sent it.
- **System Permissions settings are easier to find and understand** ([#1910](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1910)) — clearer wording and better navigation for the System Permissions section in Settings, so you know exactly what the app can touch.

### Security

- **Linux updates are signature-verified before they install** ([#1923](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1923)) — AppImage, deb, and rpm update artifacts now carry detached Ed25519 signatures, verified against a public key bundled in the client. An update that fails verification does not install. No exceptions.
- **Signed offline UI cache is live** ([#1907](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1907), [#1908](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1908)) — when the latest UI can't be fetched, the desktop client falls back to a last-known-good copy that is cryptographically verified against an embedded public key before it runs. You get a working app; you never get unverified code.

### Changed

- **Behind-the-scenes tooling, CI, and configuration upkeep** ([#1918](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1918), [#1920](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1920)) — removed a retired sign-in configuration value from internal checks and documentation.

## [0.2.12] — 2026-06-27

Your camera and screen share stay live when the call switches video quality layers, instead of dropping for a moment each time.

### Fixed

- **Your camera and screen share stay live when video quality layers switch** ([#1903](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1903)) — re-negotiating video layers mid-call could stop the underlying capture track and take down every camera in the room. Producers now keep the track alive across the switch.
- **Age-verification status survives a reload** ([#1904](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1904)) — verified state is re-fetched from the server on startup, so if you've already verified, you won't be asked again.

### Changed

- **Behind-the-scenes tooling, configuration, and code-quality upkeep** ([#1900](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1900), [#1901](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1901), [#1906](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1906)) — engineering write-ups, groundwork for a storage-configuration rename, and static-analysis cleanup. Nothing changes in how the app behaves for you.

## [0.2.11] — 2026-06-26

Hardware video encoding is offered only when your graphics hardware actually supports it.

### Fixed

- **Hardware video encoding now matches what your GPU can actually do** ([#1879](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1879)) — Concord Voice now queries your system's supported hardware encode profiles instead of assuming a fixed codec set, so camera and screen-share encoding picks codecs your GPU genuinely accelerates.

## [0.2.10] — 2026-06-26

You can run your own Concord Voice instance. AV1 video no longer black-screens in end-to-end encrypted calls.

### Added

- **Run your own Concord Voice instance** ([#1892](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1892)) — a guided installer walks you through standing up a self-hosted instance, and a new `concord-selfhost` command starts, stops, monitors, and health-checks the stack.

### Fixed

- **AV1 video no longer black-screens in end-to-end-encrypted calls** ([#1896](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1896)) — AV1 camera and screen-share video could fail to decrypt, leaving you staring at a black screen. We reworked per-frame media encryption (frame crypto v4) so AV1 streams decrypt reliably — encryption stays on, and your video stays visible.

### Changed

- **Behind-the-scenes tooling, CI, and dependency upkeep** ([#1877](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1877), [#1894](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1894)) — refreshed our supply-chain threat indicators and taught macOS release builds to retry a transient DMG packaging flake.

## [0.2.9] — 2026-06-24

Video no longer black-screens after an encryption key rotates mid-call, and the offline interface cache is signed so a tampered copy cannot load.

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

Voice and video got steadier: audio devices switch mid-call, direct-message calls open the call view with sound, and encrypted video keeps up with layered streams. Older direct-message attachments decrypt correctly again.

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

Images open in a proper lightbox, and invite links can be public. Camera quality adapts in group calls, and video recovers on its own once an encryption key catches up.

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

The message composer shows how many characters you have left, and handles going over the limit without losing what you typed.

### Added

- **Live message character-limit counter with overflow handling** ([#1709](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1709)) — see exactly how much room you have left as you type. The composer counts characters against your account's message limit and flags over-limit text before you send — no more surprise rejections.

### Changed

- **Behind-the-scenes tooling, CI, and documentation upkeep** ([#1780](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1780), [#1781](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1781), [#1764](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1764), [#1783](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1783)) — build-pipeline and security-scanning maintenance, documentation cleanups, and a routine supply-chain threat-list refresh. Nothing changes in how Concord Voice behaves for you.

## [0.2.2] — 2026-06-22

Premium features show what they unlock and can be redeemed with a code. Voice audio crackle is fixed, and reconnecting after a server deploy no longer needs a restart.

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

Friend categories also arrive: organize your direct messages into groups with your own emoji and colours. The grouping is encrypted with your key, so the server stores it without being able to read it.

### Fixed

- **Friend categories in direct messages** ([#1704](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1704)) — group your direct messages into categories with your own emoji and colours. The grouping is stored encrypted with your key, so the server keeps it without being able to read it.
- **macOS `.dmg` installer now attached to releases** ([#1722](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/1722)) — the release-asset `find` glob in `build-desktop.yml` (feeding `gh release create`) omitted `*.dmg`, so the notarized installer was missing from the v0.2.0 GitHub Release and the public mirror. The DMG is now attached and normalized to `ConcordVoice-<version>-macos-<arch>.dmg`, consistent with the `.zip`. The `latest-mac.yml` auto-updater manifest deliberately remains `.zip`-only (Squirrel.Mac cannot auto-update from a DMG).

## [0.2.0-Beta] — 2026-06-20 (Phase 2 — Beta release)

> Release-level rollup of Phase 2A + Phase 2B work. Per-revision detail lives in the `[0.1.12]`–`[0.1.18]` entries below; this entry surfaces the user-visible themes that close the v0.2.0-Beta milestone.

The Beta release. Sign in with Google or Apple, add two-step verification or a passkey, and recover an account you have locked yourself out of. Roles and permissions arrive alongside GIFs, server moderation, and end-to-end encrypted attachments.

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
