# Desktop Client Tests

Test suite for the Concord Voice Desktop Client (Electron/React/TypeScript).

Test files span unit, component, hook, service, shared, and E2E categories. See [[internal] Key Counts](../..[internal]#key-counts-2026-06-23) for current count.

## Structure

```text
tests/
├── setup.ts                        # Global test setup (jest-dom, crypto polyfill, window.electron mock)
├── test-utils.tsx                  # Custom render() with BrowserRouter, re-exports Testing Library
├── helpers/
│   ├── store-helpers.ts            # resetAllStores() for all 41 Zustand stores
│   └── crypto-helpers.ts           # mockE2EEService() stubs
├── mocks/
│   ├── fixtures.ts                 # Typed mock data: mockUser, mockServer, mockChannel, mockMessage, mockMember
│   ├── handlers.ts                 # MSW v2 request handlers (auth, servers, channels, messages, users, members)
│   └── server.ts                   # MSW setupServer()
├── unit/
│   ├── stores/                     # 11 files — Zustand store tests
│   │   ├── authStore.test.ts
│   │   ├── chatStore.test.ts
│   │   ├── serverStore.test.ts
│   │   ├── channelStore.test.ts
│   │   ├── memberStore.test.ts
│   │   ├── unreadStore.test.ts
│   │   ├── userStore.test.ts
│   │   ├── userStore.changePassword.test.ts
│   │   ├── settingsStore.test.ts
│   │   ├── inviteStore.test.ts
│   │   └── layoutStore.test.ts
│   ├── components/                 # 39 files — React component tests
│   │   ├── Auth/                   # AuthFlow, ConnectionSelector, InfoTooltip, LoadingSpinner, Login, PasswordStrength, Register, ServerInput
│   │   ├── Chat/                   # ChatView, DeleteMessageModal, Message, MessageContextMenu, MessageInput, MessageInputContextMenu, MessageList, TypingIndicator
│   │   ├── Channels/               # ChannelContextMenu, ChannelList, CreateChannelModal, DeleteChannelModal, EditChannelModal, ServerActionBar
│   │   ├── Servers/                # CreateServerModal, DeleteServerModal, JoinServerModal, LeaveServerModal, ServerActionModal, ServerContextMenu, ServerList
│   │   ├── Members/                # MemberContextMenu, MemberList, MemberProfileCard
│   │   ├── Layout/                 # AppLayout, ChannelPanel, FolderBar, MemberFlexSpace, ServerBar
│   │   ├── User/                   # UserPanel, UserPopover
│   │   ├── Settings/               # SettingsPage
│   │   ├── Profile/                # ProfilePage
│   │   ├── MainView/               # MainView
│   │   ├── ConnectionStatus/       # ConnectionStatus
│   │   ├── ui/                     # ContextMenu, Modal
│   │   └── App.test.tsx
│   ├── hooks/                      # 4 files — Custom hook tests
│   │   ├── useChannelSubscription.test.ts
│   │   ├── useMessaging.test.ts
│   │   ├── useResizablePanel.test.ts
│   │   ├── useServerChannelSubscriptions.test.ts
│   │   └── useWebSocket.test.ts
│   ├── services/                   # 5 files — Service tests
│   │   ├── apiClient.test.ts       # Auth header injection, 401 refresh retry, token clearing
│   │   ├── e2eeService.test.ts     # Initialize, channel key caching, encrypt/decrypt
│   │   ├── messageQueue.test.ts    # Enqueue, status transitions, max retries, queue limits
│   │   ├── preferencesSync.test.ts
│   │   └── websocketService.test.ts
│   └── utils/
│       └── crypto.test.ts          # Key derivation, key pair generation, wrap/unwrap, encrypt/decrypt (uses real Node webcrypto)
└── e2e/                            # Playwright E2E tests
    ├── helpers.ts                  # registerUser, loginUser, createServer, createChannel helpers
    ├── auth.spec.ts
    ├── servers.spec.ts
    ├── channels.spec.ts
    ├── messaging.spec.ts
    └── invites.spec.ts
```

## Running Tests

```bash
cd client/desktop

# Run all unit tests
npx vitest run

# Watch mode (re-runs on file changes)
npx vitest

# Run with coverage report
npx vitest run --coverage

# Run specific test file
npx vitest run tests/unit/stores/chatStore.test.ts

# Run tests matching a pattern
npx vitest run -t "renders login form"

# Run a specific directory
npx vitest run tests/unit/components/Auth/

# Run E2E tests (requires running dev server + backend)
npx playwright test
```

## Test Tools

| Tool                            | Purpose                                                  |
| ------------------------------- | -------------------------------------------------------- |
| **Vitest**                      | Test runner (Vite-native, jest-compatible API)           |
| **@testing-library/react**      | Component rendering and querying                         |
| **@testing-library/jest-dom**   | Custom DOM matchers (`toBeInTheDocument`, etc.)          |
| **@testing-library/user-event** | Realistic user interaction simulation                    |
| **MSW v2**                      | API mocking for store tests (Mock Service Worker)        |
| **jsdom**                       | Browser environment simulation                           |
| **@vitest/coverage-istanbul**   | Istanbul-based code coverage (avoids OOM vs v8 provider) |
| **Playwright**                  | E2E browser testing                                      |

## Testing Patterns

### Component Tests

Components are tested with the custom `render()` from `test-utils.tsx` which wraps with `BrowserRouter`. Zustand stores are set directly via `setState()` for setup and read via `getState()` for assertions.

### Store Tests

Zustand stores are tested directly — call actions, assert state changes. MSW intercepts API calls made by async actions. Stores are reset between tests.

### Mocking

- **`vi.mock()`** for module mocks (crypto, e2eeService, preferencesSync)
- **`vi.stubGlobal('fetch', mockFetch)`** for direct fetch mocking in component submission tests
- **MSW** for store-level API mocking (avoids `credentials: 'include'` hang in jsdom)
- **`vi.fn()`** for callback props and function mocks

### Known Limitations

- `window.matchMedia` is unavailable in jsdom — system theme tests must be skipped
- `InviteToServerModal` causes OOM in jsdom — excluded from test suite
- Components that fetch in `useEffect` on mount will override manually-set store state — mock the fetch function or API client

## CI/CD

Tests run in GitHub Actions via `.github/workflows/build.yml`, invoked via `workflow_call` from `pr-ci.yml` on every PR. The workflow runs desktop tests in parallel with control-plane and media-plane checks, then uploads coverage to SonarQube for Quality Gate enforcement.

### E2E (Playwright)

The 8 e2e specs in `tests/e2e/` run **manually** via `npm run test:e2e` — CI enforcement was removed in #1435 (advisory-only signal, macOS visual-baseline flakiness; see the historical playwright ADR-0011).

- **Renderer-only specs** (`visual-regression`, `design-tokens`, `bundled-fallback-login`) — tagged with `{ tag: '@renderer-only' }`. Run on every PR. Need only the Vite dev server.
- **Full-stack specs** (`auth`, `channels`, `invites`, `messaging`, `servers`) — untagged (default). Run ONLY when the PR also touches `services/control-plane/**` or migrations. Need a running backend (Postgres + Redis + control-plane).

The workflow uses `services:` blocks for Postgres + Redis; the control-plane Go binary starts via `go run ./cmd/server &` conditionally based on `dorny/paths-filter` output.

**Visual-regression failure semantics:** non-blocking. A snapshot diff against the committed baseline posts a sticky PR comment with the diff artifact link, but the workflow exits 0 (green CI). Real test failures (assertion, timeout, navigation) exit 1 (red CI).

**Re-baselining:** the canonical Linux baselines are captured + committed by a one-shot bot-authored follow-up PR after #1074 merges. The original Mac-captured `*-chromium-darwin.png` baselines remain in the tree until that PR merges; PRs in the meantime will see non-blocking drift comments from Mac→Linux subpixel rendering differences.

To run e2e locally (renderer-only specs, no backend needed):

```bash
cd client/desktop
npx playwright test --grep @renderer-only
```

To run e2e locally (all specs, requires backend running at `http://localhost:8080`):

```bash
# In a separate shell, start the backend stack
./scripts/concord-dev.sh up

# Then run e2e
cd client/desktop
npx playwright test
```

Pre-commit hooks (`./scripts/install-git-hooks.sh`) provide local linting and type-checking before push.

Coverage target: **80%+** on new code (enforced by SonarQube Quality Gate).
