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

- Node.js 20+
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
│       ├── stores/    # State management (41 Zustand stores)
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

The preload script (`src/preload/preload.ts`) provides a secure bridge between main and renderer processes using `contextBridge`.

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

Do not launch packaged Linux builds with `--no-sandbox`. Electron's `chrome-sandbox` helper is expected to ship with SUID mode `4755`; the CI package verifier checks this for DEB and RPM artifacts.

### Google SSO build constant (`GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP`)

Google Sign-In uses a client-driven PKCE exchange (#975): the desktop main
process performs Google's `/token` exchange itself, so it embeds Google's
OAuth `client_secret`. Per Google's [OAuth 2.0 for Native Apps guidance](https://developers.google.com/identity/protocols/oauth2/native-app),
a "Desktop application" client's `client_secret` is **not confidential** — PKCE
(`code_challenge_method=S256`) is the actual security control. The value is
injected at package time by `npm run build:gclientsecret` (from the
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
- Video and screen sharing UI
- DM system with conversations and voice calls
- Friend codes and privacy controls
- Emoji picker (1800+ emoji, search, skin tones, recently used)
- Channel groups with drag-and-drop ordering
- Custom context menus
- Profile popovers
- **41 Zustand stores** (Zustand 5.0.12) for state management — see [STATE_MANAGEMENT.md](docs/STATE_MANAGEMENT.md) and [internal] for authoritative counts

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
- [ ] File uploads, @mention tagging
- [ ] Push-to-talk with global shortcuts
- [ ] System tray integration
- [ ] Auto-updates
- [ ] Native notifications
- [ ] Lazy loading and code splitting
