# Concord Voice Product Overview

> Summary for AI assistants. For roadmap details and current counts (routes, migrations, stores), see [[internal]](.[internal]).

## What Is Concord Voice?

Concord Voice is a **privacy-first, hybrid SaaS + self-hosted** real-time communications platform. It delivers the real-time chat, voice, and video people expect — with end-to-end encryption by default and the option to self-host your own instance.

**Organization:** Concord Voice LLC
**License:** CVSL 1.0 (source-available; converts to AGPL-3.0-or-later on 2030-02-15)
**Domains:** example.com (infrastructure), concordvoice.com (public-facing)

<!-- audit-exempt: historical reference (v0.1.0-Alpha is a shipped versioned identifier) -->
## Core Features (v0.1.0-Alpha + Phase 2B Shipped)

### Text Communication
- Server-based channels with real-time messaging via WebSocket
- Direct messages (1:1 and group) with full E2EE support
- Channel groups for organization
- Message editing and deletion
- Message reactions, reply/quote threading
- DM message pinning
- Emoji picker
- GIF integration via Klipy privacy proxy (no third-party tracking)
- Read state tracking per channel and DM

### Voice & Video
- Voice channels with mediasoup WebRTC SFU
- Opus codec with 7 quality tiers
- Per-user volume control, mute/deafen, push-to-talk
- Noise suppression
- Screen sharing (screen/window picker)
- Video calls with camera toggle
- DM voice calls

### Security & Privacy
- End-to-end encryption (AES-256-GCM) for channels and DMs
- RSA-OAEP 4096-bit key wrapping with epoch-based rotation
- MFA support (TOTP + WebAuthn/passkeys)
- Argon2id password hashing
- Electron safeStorage for credential storage (OS keychain)
- Email verification

### Server Management
- Server creation with customizable icons, banners, themes
- Channel groups with drag-and-drop ordering
- Role-based access control (RBAC) with hierarchical permissions
- Server invites with expiration and usage limits
- Ownership transfer with MFA verification
- Server-level mute and deafen controls
- 12 color schemes (dark/light variants)

### Desktop Client
- Cross-platform Electron app (macOS, Windows, Linux)
- Auto-updater with splash screen, safety checks, rollback
- Compact mode and resizable panels

## Planned Features (v0.2.0-Beta — In Progress)

### Phase 2B: Core Features & Polish (In Progress)

- ✅ Message reactions, replies/quotes (shipped)
- ✅ DM message pinning (shipped)
- ✅ GIF integration via Klipy privacy proxy (shipped)
- ✅ Server mute/deafen (shipped)
- File and image attachments (object storage ready)
- Profile photo/banner crop editors
- Desktop notifications with @mention routing
- Draft message persistence
- Keyboard shortcuts
- Per-participant volume sliders
- Extended markdown rendering
- Frontend performance optimization (lazy-load, code-split)
- Friend request UI improvements

## Future Features (v1.0.0)

- Mobile clients (Android + iOS)
- Self-hosted server installer
- Monetization and subscription tiers
- E2EE enforcement everywhere
- Privacy-first presence control
- Screen share remote control and annotations
- Webhooks, bots, and developer resources
- Bug report and feature request panels

## User Roles

- **Server Owner:** Full control, can transfer ownership
- **Administrators:** Manage channels, roles, members (via RBAC)
- **Moderators:** Manage messages, mute users (via RBAC)
- **Members:** Standard access based on role permissions
- **Guests:** Limited access (future)

## Target Audience

Privacy-conscious communities, gaming groups, development teams, and organizations that want full-featured real-time chat and voice with E2EE and the option to self-host for data sovereignty.
