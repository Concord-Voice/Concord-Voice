# Desktop Client Tests

Test suite for the Concord Voice Desktop Client (Electron/React/TypeScript).

Test files span unit, component, hook, service, shared, and E2E categories. CI reports current test status and coverage.

## Structure

The map below is one level deep. It shows where a test belongs, not what each directory holds.
Run `ls tests/unit/` for the current set.

```text
tests/
├── setup.ts                # Global test setup (jest-dom, crypto polyfill, window.electron mock)
├── test-utils.tsx          # Custom render() with BrowserRouter, re-exports Testing Library
├── helpers/                # resetAllStores(), crypto stubs, WS/BroadcastChannel mocks, deferred
├── mocks/                  # MSW v2 handlers, setupServer(), typed fixtures
├── __mocks__/              # Vitest automock directory
├── fixtures/               # Static test data
├── integration/            # Cross-module tests (fan-out, pinning, preload sandbox contract)
├── unit/
│   ├── build/              # Build-config and packaging assertions
│   ├── components/         # React component tests
│   ├── functions/          # Cloudflare Worker function tests
│   ├── hooks/              # Custom hook tests
│   ├── main/               # Electron main-process tests
│   ├── renderer/           # Renderer-side modules outside components/hooks/services
│   ├── scripts/            # Repo script tests
│   ├── services/           # Service-layer tests
│   ├── shared/             # Code shared between main and renderer
│   ├── stores/             # Zustand tests; store-reset-coverage.test.ts guards resetAllStores() parity
│   ├── styles/             # Theme and CSS token tests
│   ├── types/              # Type-level and schema tests
│   ├── utils/              # Pure utility tests
│   ├── workers/            # Web Worker tests
│   ├── csp-allowlist.test.ts
│   ├── csp-policy.test.ts
│   ├── csp-prod-strip.test.ts
│   └── no-sentry-imports.test.ts
└── e2e/                    # Playwright E2E specs + helpers.ts
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

Component tests use the custom `render()` from `test-utils.tsx`, which wraps with `BrowserRouter`. Tests set Zustand stores directly via `setState()` for setup, and read them via `getState()` for assertions.

### Store Tests

Store tests call actions directly and assert state changes. MSW intercepts API calls from async actions. Each test resets the stores.

### Mocking

- **`vi.mock()`** for module mocks (crypto, e2eeService, preferencesSync)
- **`vi.stubGlobal('fetch', mockFetch)`** for direct fetch mocking in component submission tests
- **MSW** for store-level API mocking (avoids `credentials: 'include'` hang in jsdom)
- **`vi.fn()`** for callback props and function mocks

### Known Limitations

- jsdom does not implement `window.matchMedia`. Stub it in `beforeAll` — see `tests/unit/stores/settingsStore.extended.test.ts`
- Components that fetch in `useEffect` on mount will override manually-set store state. Mock the fetch function or the API client

## CI/CD

Tests run in GitHub Actions via `.github/workflows/build.yml`, invoked via `workflow_call` from `pr-ci.yml` on every PR. The workflow runs desktop tests in parallel with control-plane and media-plane checks. It then uploads coverage to SonarQube for Quality Gate enforcement.

### E2E (Playwright)

The 10 e2e specs in `tests/e2e/` run **manually** via `npm run test:e2e`. #1435 removed CI enforcement, because the signal was advisory-only and macOS visual baselines were flaky. See the historical playwright ADR-0011.

No Playwright CI job exists for the desktop client — that change deleted `playwright.yml`, so nothing below runs on a PR. The only Playwright in CI is the Admin Portal's own browser-test step in `build.yml` (`.github/workflows/build.yml:717-723`). The tags below still help a local run:

- **Renderer-only specs** (`visual-regression`, `design-tokens`, `bundled-fallback-login`) — tagged with `{ tag: '@renderer-only' }`. Need only the Vite dev server.
- **Full-stack specs** (`auth`, `channels`, `invites`, `messaging`, `servers`) — untagged (default). Need a running backend (Postgres + Redis + control-plane).

The committed visual baselines are Mac-captured (`*-chromium-darwin.png` in `tests/e2e/visual-regression.spec.ts-snapshots/`). `visual-regression.spec.ts` therefore only produces meaningful diffs on macOS.

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

Pre-commit hooks (`./scripts/install-git-hooks.sh`) run local linting and type-checking before push.

Coverage target: **80%+** on new code, which the SonarQube Quality Gate enforces.
