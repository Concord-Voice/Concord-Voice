# Concord Voice Desktop Client

Electron-based desktop application for Concord voice communication platform.

## Tech Stack

- **Electron** - Cross-platform desktop framework
- **React** - UI library
- **TypeScript** - Type-safe development
- **Vite** - Fast build tool and dev server
- **WebRTC** - Real-time voice communication

## Development

### Prerequisites

- Node.js 24.15+ (see `engines` in `package.json`)
- npm 10+

### Setup

```bash
# Install dependencies
npm install

# Run in development mode
npm run dev
```

The app will start with hot-reload enabled. The renderer process runs on `http://localhost:3001`.

### Available Scripts

- `npm run dev` - Start development mode with hot reload
- `npm run build` - Build for production
- `npm run package` - Package the app for distribution
- `npm run make` - Create platform-specific installers
- `npm run lint` - Run ESLint with the same effective scope as the pre-commit hook.
  Reports the same findings you would see at `git commit`. Excludes `tests/`
  (which has its own type-check pipeline via Vitest).
- `npm run typecheck` - Run TypeScript type checking

## Project Structure

```
desktop/
├── src/
│   ├── main/           # Electron main process
│   │   └── main.ts    # App entry point, window management
│   ├── preload/       # Preload scripts (IPC bridge)
│   │   └── preload.ts
│   ├── shared/        # Cross-process modules (main + renderer)
│   └── renderer/      # React application
│       ├── components/ # React components
│       ├── hooks/     # Custom React hooks
│       ├── stores/    # State management (Zustand)
│       ├── utils/     # Utility functions
│       ├── styles/    # CSS styles
│       ├── App.tsx    # Main app component
│       └── main.tsx   # React entry point
├── index.html         # HTML template
├── package.json
├── tsconfig.json      # TypeScript config (renderer)
├── tsconfig.main.json # TypeScript config (main)
└── vite.config.ts     # Vite configuration
```

## Architecture

### Main Process

The main process (`src/main/main.ts`) handles:

- Window creation and management
- System integration
- IPC communication with renderer

### Preload Script

The preload script (`src/preload/preload.ts`) is a secure bridge between main and renderer processes, built with `contextBridge`.

### Renderer Process

The renderer process is a React application that handles:

- UI rendering
- User interactions
- WebRTC connections
- State management

## Security

- **Context Isolation**: Enabled to prevent renderer access to Node.js
- **Sandbox**: Enabled for additional security
- **CSP**: Content Security Policy configured in HTML
- **No Node Integration**: Disabled in renderer for security
- **IPC**: Only whitelisted APIs exposed via preload script

## Building for Production

```bash
# Build the app
npm run build

# Create distributable packages
npm run make
```

This will create platform-specific installers in the `out/` directory.

### Linux packages

Install the Debian package from the directory containing the downloaded file:

```bash
sudo apt install ./concord-voice_<version>_linux-x64.deb
```

If apt prints a note like `Download is performed unsandboxed as root ... couldn't be accessed by user '_apt'`, that is informational when `Setting up concord-voice (...)` completes. It means apt's sandbox user could not read the local file path, not that the package failed to install.

After install, the launcher entry should appear as **Concord Voice** with the Concord icon. If a desktop environment does not refresh its menu immediately, log out/in or run the desktop's menu refresh command.

The AppImage is the fallback when a DEB/RPM install is not desirable:

```bash
chmod +x ConcordVoice-<version>-linux-x64.AppImage
./ConcordVoice-<version>-linux-x64.AppImage
```

Do not launch packaged Linux builds with `--no-sandbox`. Electron's `chrome-sandbox` helper must ship with SUID mode `4755`. The CI package verifier checks this for DEB and RPM artifacts.

### Google SSO build constant (`GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP`)

Google Sign-In uses a client-driven PKCE exchange (#975). The desktop main
process runs Google's `/token` exchange itself, so it embeds Google's
OAuth `client_secret`. Per Google's [OAuth 2.0 for Native Apps guidance](https://developers.google.com/identity/protocols/oauth2/native-app),
a "Desktop application" client's `client_secret` is **not confidential**. PKCE
(`code_challenge_method=S256`) is the actual security control.

`npm run build:gclientsecret` injects the value at package time (from the
`GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP` env var / CI repo secret) into a
main-process-only `googleClientSecret.json` resource, read by
`src/main/oauth/google/clientSecret.ts`. It is deliberately **not** a
`VITE_`-prefixed variable, so it never enters the renderer bundle. A build
without the value produces an inert Google SSO (empty secret), not a failure.

## Features

### Phase 1A: Authentication & E2EE ✅

- User registration with E2EE key generation (RSA-OAEP **4096-bit**)
- Login with automatic key unwrapping (Argon2id key derivation)
- Token management (access + refresh tokens)
- Session management with device tracking
- Password strength meter (5-tier system)
- Username validation with profanity filtering
- Secure key storage (private keys never sent to server)
- Connection selector (hosted vs self-hosted)

### Phase 1B: Channels & Text Chat ✅

- Server creation/management with icons and header images
- Channel list with E2EE indicators
- Real-time messaging via WebSocket (ticket-based auth)
- Message editing, deletion, date dividers
- Presence indicators (online/offline/typing)
- Server invites with code sharing
- Channel read states and unread badges
- Custom themes (32 theme blocks, 15 color schemes x dark/light + root + base light)
- Compact mode, resizable panels, font scaling

### Phase 1C: Voice & Media ✅

- WebRTC voice channels via mediasoup SFU
- In Settings → Audio & Video, Audio Configuration starts with Input and Output selectors (including Default), microphone and speaker tests, and output level; Video Configuration starts with Camera selection, Test, and preview. Device choices apply immediately; processing and Microphone Level use Apply/Revert. Microphone Level is app-only, defaults to 100%, and adjusts input from 0–200% in 1% steps when effective automatic gain control (AGC) is off, including Music Mode. Its saved value is retained while hidden and applied before the saved noise gate in calls and Microphone Test. While AGC manages the microphone, positive saved levels are ignored and app gain stays at unity; a saved 0% remains silent. The saved level applies again when AGC is off or Music Mode is on. Boosting can amplify noise and clip audio; clipping already present at the input cannot be repaired downstream. Test uses applied settings, so select Apply and retest the gate after changing level.
- Calls and Microphone Test use the same processing order: app microphone level, AGC-only peak protection, then the noise gate. Noise Gate offers Dynamic, Manual Calibrate, and Off; with AGC active, Dynamic and Off are available. Dynamic adapts to changing microphone levels during an active call or Test and stays permissive when background noise and speech cannot be separated by level. A single short elevated sound does not establish a threshold; recurring short level changes can. Level patterns alone cannot identify the speaker. After a sustained quiet stretch without an elevated level, it returns to permissive learning so a stale threshold does not keep cutting off softer speech; background sound may pass during relearning. Manual Calibrate lets you set the fixed dBFS threshold yourself; check it again if your room, microphone position, hardware, processing, or Microphone Level changes. Off leaves the gate open. The guided Auto Calibrate test is planned separately. With AGC active, the app limits outgoing sample peaks to −6 dBFS at the worklet output; it uses a 4 ms lookahead and 100 ms recovery, which add about 4 ms of processing delay. The limit does not measure acoustic loudness, prevent clipping already present in the source, or cover later processing outside this path. A worklet load or processing failure never falls back to unprotected AGC audio: the affected path pauses or stops and reports a retry error. Microphone Test shows post-protection, pre-gate peaks and warns when its input reaches full scale before the gate; that warning can indicate clipping but does not diagnose its source. Test reflects applied settings and stops on section close/collapse or call conflict; changing the selected input replaces its active test capture, while output or camera changes stop their tests.
- Audio **Default** follows your operating system's default input or output device; choosing a named device keeps that device selected. If Concord cannot open the selected microphone, choose another input or close the app using it, then try again. Voice-join errors appear in a small centered prompt without moving the toolbar or conversation; dismiss it with Dismiss or Escape. For permission failures, check microphone access in system settings. Concord does not silently switch microphones.
- Audio setting changes preserve your mute, including when you press Mute during a microphone update. Changing an input or audio setting can retry a failed microphone update during a call. Speaking feedback keeps working after switching or rebuilding the microphone, including while alone in a call.
- Video and screen sharing UI
- DM system with conversations and voice calls
- Friend codes and privacy controls
- Emoji picker (1800+ emoji, search, skin tones, recently used)
- Channel groups with drag-and-drop ordering
- Custom context menus
- Profile popovers
- **Zustand stores** (Zustand 5.0.12) for state management. See [STATE_MANAGEMENT.md](docs/STATE_MANAGEMENT.md) and `src/renderer/stores/` for the current set

### Security ✅

- Electron hardening: safeStorage, ASAR, sandbox, context isolation
- Secure IPC via preload scripts (no @electron/remote)
- CSP configured
- E2EE: AES-256-GCM message encryption, RSA-OAEP 4096-bit key wrapping
- **Developer mode toggle** — in-app flag for enabling diagnostic tooling without a production build

### Phase 2+ 📋

- [x] GIF integration (Klipy, savedGifsStore, privacy controls)
- [x] DM message pinning (migration 000057)
- [x] Server mute/deafen (migration 000054)
- [x] Message reactions
- [x] Reply/quote messages
- [x] File uploads, @mention tagging
- [x] System tray integration
- [x] Auto-updates
- [x] Native notifications
- [x] Lazy loading and code splitting
- [ ] Push-to-talk with global shortcuts
