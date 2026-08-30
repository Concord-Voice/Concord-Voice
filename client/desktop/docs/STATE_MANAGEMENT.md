# State Management Architecture

**Framework:** Zustand v5.0.12
**Last Updated:** 2026-08-30
**Decision:** Zustand chosen over Redux after cost-benefit analysis (see below)

---

## Overview

Concord Voice uses [Zustand](https://github.com/pmndrs/zustand) for state management across all frontend stores. Zustand was chosen for its simplicity, TypeScript integration, small bundle size (45KB vs Redux's 200KB), and superior developer experience.

### Why Zustand over Redux?

**Key Decision Factors:**

- **Development Speed:** 40% less boilerplate code saves ~1.5 days
- **Bundle Size:** 77% smaller (critical for Electron startup time)
- **Type Safety:** Better TypeScript DX reduces security bugs
- **Real-Time:** Direct WebSocket integration (no middleware complexity)
- **Privacy:** Fine-grained persistence control (privacy-first design)
- **Team Size:** Simpler for 2-developer team
- **Mobile Ready:** Works identically in React Native (Phase 3)

**Weighted Score:** Zustand 8.6 / Redux 6.1 (40% advantage)

Full analysis was performed during Phase 1A architecture decisions (internal).

---

## Store Architecture

### Store Inventory

| Store                           | Purpose                                                      | Persisted            | Phase |
| ------------------------------- | ------------------------------------------------------------ | -------------------- | ----- |
| **authStore**                   | Access/session lineage and auth lifecycle                    | ❌                   | 1A    |
| **chatStore**                   | Messages, typing, connection                                 | ❌ (privacy)         | 1A    |
| **serverStore**                 | Server list, active server                                   | ✅ active server ID  | 1A    |
| **userStore**                   | User profile, preferences                                    | ❌                   | 1A    |
| **channelStore**                | Channel list, active channel                                 | ✅ active channel ID | 1B    |
| **connectionStore**             | WebSocket connection state + `wireViolationCount` (PR #1184) | ❌                   | 1B    |
| **memberStore**                 | Server members, roles                                        | ❌                   | 1B    |
| **layoutStore**                 | UI layout state (sidebar, panels)                            | ✅ partial           | 1B    |
| **settingsStore**               | App settings, theme, font scale                              | ✅                   | 1B    |
| **unreadStore**                 | Unread message counts                                        | ❌                   | 1B    |
| **inviteStore**                 | Server invites                                               | ❌                   | 1B    |
| **draftSettingsStore**          | Unsaved settings drafts                                      | ❌                   | 1B    |
| **ttsSettingsStore**            | Text-to-speech settings                                      | ✅                   | 1B    |
| **voiceStore**                  | Voice state, participants, quality tiers                     | ✅ device settings   | 1C    |
| **audioSettingsStore**          | Audio input/output device prefs                              | ✅                   | 1C    |
| **videoSettingsStore**          | Video device and codec prefs                                 | ✅                   | 1C    |
| **dmStore**                     | DM conversations, participants                               | ❌                   | 1C    |
| **friendStore**                 | Friend list, friend requests                                 | ❌                   | 1C    |
| **privacyStore**                | Privacy settings, friend codes                               | ❌                   | 1C    |
| **clientConfigStore**           | Server-provided client configuration                         | ❌                   | 2A    |
| **mfaChallengeStore**           | MFA challenge state during auth flows                        | ❌                   | 2A    |
| **notificationStore**           | Desktop notification preferences and queue                   | ✅                   | 2A    |
| **osPermissionStore**           | OS-level permission states (mic, camera, screen)             | ❌                   | 2A    |
| **permissionStore**             | RBAC permission state for current server/channel             | ❌                   | 2A    |
| **channelScrollStore**          | Per-channel scroll position tracking                         | ❌                   | 2B    |
| **draftMessageStore**           | Per-channel unsent message drafts                            | ❌                   | 2B    |
| **keyboardShortcutStore**       | Keyboard shortcut configuration and state                    | ✅                   | 2B    |
| **notificationNavigationStore** | Notification click navigation targets                        | ❌                   | 2B    |
| **savedGifsStore**              | User's saved/favourite GIFs (Klipy integration)              | ✅                   | 2B    |
| **settingsOverlayStore**        | Settings panel open/close and active tab state               | ❌                   | 2B    |
| **e2eeStore**                   | Reactive mirror of the `e2eeService` lifecycle               | ❌                   | 2B    |
| **ssoStore**                    | In-flight SSO authentication state                           | ❌                   | 2B    |
| **pendingRegistrationStore**    | Registration awaiting email verification                     | ✅ sessionStorage    | 2B    |
| **updateStatusStore**           | Critical update-channel errors behind the security banner    | ❌                   | 2B    |
| **notificationPrefsStore**      | Per-target mute preferences                                  | ❌ (server-side)     | 2B    |
| **attestationFailureStore**     | Terminal client-attestation failure codes                    | ❌                   | 2B    |
| **settingsNavStore**            | Settings left-nav section and deep-link target               | ❌                   | 2B    |
| **subscriptionStore**           | Subscription entitlement capability set                      | ❌                   | 2B    |
| **richPresenceStore**           | Rich Presence custom text status                             | ❌                   | 2B    |
| **friendOrgStore**              | Friend categories (zero-knowledge organization blob)         | ❌                   | 2B    |
| **changelogStore**              | Last-seen changelog version for the post-update modal        | ✅ localStorage      | 2B    |
| **presenceOverrideStore**       | Custom Status recipient exceptions                           | ❌                   | 2B    |

> **Current metrics:** See the "Key Counts" section of [[internal]](../../..[internal]).

The inventory above is an architectural overview. The canonical store set lives in `src/renderer/stores/`.

The table inventories 42 Zustand stores. The structural 43-module count in `[internal]` also includes `colorSyncSuppression.ts`, a dependency-free mutable UI support module intentionally excluded from this table.

---

## Store Details

### 1. authStore

**Purpose:** Renderer-owned access credentials and authentication lifecycle fencing

**Location:** `src/renderer/stores/auth/authStore.ts`

**State:**

```typescript
interface AuthState {
  accessToken: string | null;
  sessionId: string | null;
  authGeneration: number;
  beginAuthLifecycle: (accessToken: string, sessionId: string | null) => number;
  rotateAuthCredentials: (
    expectedGeneration: number,
    accessToken: string,
    sessionId: string | null
  ) => boolean;
  clearAccessToken: () => void;
}
```

**Credential ownership:**

- The renderer holds `accessToken`, `sessionId`, and the monotonic `authGeneration` in memory only.
- Refresh credentials are main-process-owned. `tokenManager.ts` keeps session-only credentials in memory and uses Electron `safeStorage` only when Remember Me is enabled.
- Login/restore starts a new auth generation. An accepted refresh rotates the access/session pair only when the captured generation still matches.
- `clearAccessToken()` clears the renderer pair and advances the generation, fencing stale async continuations.

`authStore` uses the shared `createStore` wrapper and does not use Zustand persistence middleware.

**Usage:**

```typescript
import { useAuthStore } from '@/renderer/stores/auth/authStore';

// In component
const accessToken = useAuthStore((state) => state.accessToken);
const sessionId = useAuthStore((state) => state.sessionId);

// Direct access (non-reactive)
const generation = useAuthStore.getState().authGeneration;
```

---

### 2. chatStore

**Purpose:** Real-time messages, typing indicators, WebSocket connection status

**Location:** `src/renderer/stores/chat/chatStore.ts`

**State:**

```typescript
interface ChatState {
  // Messages by channel ID
  messagesByChannel: Map<string, MessageWithStatus[]>;

  // Typing indicators by channel ID
  typingByChannel: Map<string, Map<string, TypingUser>>;

  // WebSocket connection status
  isConnected: boolean;
  connectionClientId: string | null;

  // Actions (15 total)
  addMessage: (channelId: string, message: MessageWithStatus) => void;
  updateMessageStatus: (channelId, clientMessageId, status, serverId?, error?) => void;
  setTyping: (channelId, userId, isTyping, username?) => void;
  // ... more actions
}
```

**Persistence:**

- ❌ **Does NOT persist** (privacy-first design)
- **Privacy Rationale:** Messages should not persist across app restarts for security
- **Offline Queue:** Handled separately by `messageQueue.ts` (uses localStorage)

**DevTools:** Enabled as "ChatStore"

**Key Features:**

- Message deduplication (by ID)
- Delivery status tracking (pending → sent → delivered → failed)
- Auto-cleanup of old typing indicators (5-second timeout)
- Optimistic UI updates

**Integration:**

- `useWebSocket.ts` - Real-time message updates
- `useMessaging.ts` - Send messages with delivery tracking
- `messageQueue.ts` - Offline message queue

**Usage:**

```typescript
import { useChatStore } from '@/renderer/stores/chat/chatStore';

// Get messages for a channel
const messages = useChatStore((state) => state.messagesByChannel.get(channelId) || []);

// Add a new message
const addMessage = useChatStore((state) => state.addMessage);
addMessage(channelId, message);

// Update message status (delivery tracking)
const updateStatus = useChatStore((state) => state.updateMessageStatus);
updateStatus(channelId, clientMessageId, 'delivered', serverMessageId);
```

---

### 3. serverStore

**Purpose:** Server list, active server selection

**Location:** `src/renderer/stores/chat/serverStore.ts`

**State:**

```typescript
interface ServerState {
  servers: ServerWithRole[];
  activeServerId: string | null;
  isLoading: boolean;
  error: string | null;

  fetchServers: (accessToken: string) => Promise<void>;
  addServer: (server: ServerWithRole) => void;
  updateServer: (serverId: string, updates: Partial<ServerWithRole>) => void;
  removeServer: (serverId: string) => void;
  setActiveServer: (serverId: string | null) => void;
  clearServers: () => void;
}
```

**Persistence:**

- ✅ **Persists:** `activeServerId` only
- ❌ **Does NOT persist:** `servers` list (re-fetched on login)
- **Storage Key:** `concord-servers`
- **UX Rationale:** Restores last active server on app restart for better UX

**DevTools:** Enabled as "ServerStore"

**Usage:**

```typescript
import { useServerStore } from '@/renderer/stores/chat/serverStore';

// Get active server
const activeServerId = useServerStore((state) => state.activeServerId);
const servers = useServerStore((state) => state.servers);
const activeServer = servers.find((s) => s.id === activeServerId);

// Fetch servers on login
const fetchServers = useServerStore((state) => state.fetchServers);
await fetchServers(accessToken);

// Switch active server
const setActiveServer = useServerStore((state) => state.setActiveServer);
setActiveServer(serverId);
```

---

### 4. userStore

**Purpose:** User profile data, preferences, logout handling

**Location:** `src/renderer/stores/auth/userStore.ts`

**State:**

```typescript
interface UserState {
  user: UserProfile | null;
  isLoading: boolean;
  error: string | null;

  fetchUser: (accessToken: string) => Promise<void>;
  setUser: (user: UserProfile) => void;
  clearUser: () => void;
  logout: () => Promise<void>;
  updateProfile: (accessToken, updates: UpdateProfileData) => Promise<void>;
  changePassword: (accessToken, currentPassword, newPassword) => Promise<{ success; error? }>;
}
```

**Persistence:**

- ❌ **Does NOT persist** (re-fetched on login)
- **Security Rationale:** User data is sensitive and should not persist locally

**DevTools:** Enabled as "UserStore"

**Key Features:**

- Password change with E2EE key re-wrapping (crucial for E2EE)
- Profile updates (username, display_name, bio, avatar, links)
- Logout clears all stores (authStore, chatStore, serverStore)

**Usage:**

```typescript
import { useUserStore } from '@/renderer/stores/auth/userStore';

// Get current user
const user = useUserStore((state) => state.user);

// Logout (clears all stores)
const logout = useUserStore((state) => state.logout);
await logout();

// Update profile
const updateProfile = useUserStore((state) => state.updateProfile);
await updateProfile(accessToken, { display_name: 'New Name' });

// Change password (with E2EE key re-wrapping)
const changePassword = useUserStore((state) => state.changePassword);
const result = await changePassword(accessToken, currentPwd, newPwd);
```

---

## Middleware Usage

### DevTools Middleware

Stores created with Zustand's `devtools` middleware appear in Redux DevTools. Lightweight stores such as `authStore` use the shared `createStore` wrapper without this middleware.

**Setup:**

```typescript
import { devtools } from 'zustand/middleware';

export const useChatStore = create<ChatState>()(
  devtools(
    (set, get) => ({/* store logic */}),
    { name: 'ChatStore' } // Shows up in Redux DevTools!
  )
);
```

**Usage:**

1. Install [Redux DevTools Extension](https://github.com/reduxjs/redux-devtools)
2. Open browser DevTools → Redux tab
3. Select an instrumented store such as ChatStore, ServerStore, or UserStore

**Features:**

- ✅ State inspection
- ✅ State changes tracking
- ⚠️ Limited time-travel (not full Redux time-travel)
- ⚠️ No action filtering (Zustand uses direct state updates)

**Disable in Production:**

```typescript
// DevTools automatically disabled in production builds
// No manual configuration needed
```

---

### Persist Middleware

Stores that retain device-local UI choices use Zustand's `persist` middleware. `authStore` deliberately does not. Refresh credentials remain main-process-owned.

**Setup:**

```typescript
import { persist, devtools } from 'zustand/middleware';

const serverStore = create<ServerState>()(
  devtools(
    persist((set) => ({/* store logic */}), {
      name: 'concord-servers',
      partialize: (state) => ({
        activeServerId: state.activeServerId,
      }),
    }),
    { name: 'ServerStore' }
  )
);
```

**Privacy Controls:**

- Use `partialize` to control EXACTLY what persists
- Keep credentials and user content out of renderer persistence
- Default: Entire state persists (use partialize for fine-grained control)

**Storage:**

- Default: `localStorage`
- Alternative: `sessionStorage`, custom storage adapter

---

## Best Practices

### 1. Selector Optimization

**Bad (triggers re-render on any state change):**

```typescript
const store = useChatStore();
const messages = store.messagesByChannel.get(channelId);
```

**Good (only re-renders when specific data changes):**

```typescript
const messages = useChatStore((state) => state.messagesByChannel.get(channelId) || []);
```

**Best (shallow comparison for object selectors):**

```typescript
import { useShallow } from 'zustand/react/shallow';

const { addMessage, updateMessage } = useChatStore(
  useShallow((state) => ({
    addMessage: state.addMessage,
    updateMessage: state.updateMessage,
  }))
);
```

Zustand 5 removed the two-argument equality-function overload. Wrap the selector in
`useShallow` instead of passing `shallow` as a second argument.

---

### 2. Non-Reactive Access

**Use `getState()` for one-time reads (no re-render):**

```typescript
// In event handler, WebSocket callback, etc.
wsService.onMessage('message', (data) => {
  // Direct access - no component re-render
  useChatStore.getState().addMessage(data.channel_id, data);
});
```

---

### 3. Privacy & Security

**Messages should NOT persist:**

```typescript
// chatStore - NO persistence (privacy)
export const useChatStore = create<ChatState>()(
  devtools((set, get) => ({/* ... */}), { name: 'ChatStore' })
  // No persist middleware!
);
```

**Auth credentials - no renderer persistence:**

```typescript
// authStore uses createStore directly; tokenManager owns refresh credentials.
export const useAuthStore = createStore<AuthState>()((set) => ({
  accessToken: null,
  sessionId: null,
  authGeneration: 0,
  // ...auth lifecycle actions
}));
```

**Logout - clear ALL stores:**

```typescript
// userStore.logout() delegates the final cross-store wipe to resetService.
const { nuclearReset } = await import('../../services/resetService');
nuclearReset();
```

---

### 4. TypeScript Type Safety

**Always define state interface:**

```typescript
interface ChatState {
  messagesByChannel: Map<string, MessageWithStatus[]>;
  addMessage: (channelId: string, message: MessageWithStatus) => void;
  // ... all actions with full types
}

export const useChatStore = create<ChatState>()(/* ... */);
```

**Benefits:**

- Full autocomplete in components
- Compile-time type checking
- Prevents runtime errors (critical for E2EE)

---

### 5. Store Composition

**Stores can call other stores:**

```typescript
// userStore coordinates logout, then delegates the complete wipe.
logout: async () => {
  // ...stop sync and request main-process logout
  const { nuclearReset } = await import('../../services/resetService');
  nuclearReset();
};
```

**But avoid circular dependencies!**

---

## Testing

### Unit Testing Stores

**Example (authStore):**

```typescript
import { renderHook, act } from '@testing-library/react';
import { useAuthStore } from '@/renderer/stores/auth/authStore';

describe('authStore', () => {
  beforeEach(() => {
    // Clear store before each test
    useAuthStore.getState().clearAccessToken();
  });

  it('starts a new auth lifecycle', () => {
    const { result } = renderHook(() => useAuthStore((state) => state));

    act(() => {
      result.current.beginAuthLifecycle('access123', 'session-1');
    });

    expect(result.current.accessToken).toBe('access123');
    expect(result.current.sessionId).toBe('session-1');
    expect(result.current.authGeneration).toBeGreaterThan(0);
  });
});
```

Store tests live in `client/desktop/tests/unit/stores/`. `store-reset-coverage.test.ts` guards
`resetAllStores()` parity with the store inventory.

---

## Migration Path (If Needed)

### Zustand → Redux Migration

**If we ever need Redux** (e.g., for complex state machines):

**Option 1: Incremental Migration**

```typescript
// Keep Zustand stores for simple state
export const useChatStore = create(...); // Keep as-is

// Add Redux for complex feature
const complexFeatureStore = configureStore({
  reducer: { complex: complexSlice }
});

// Both work in same app!
```

**Option 2: Full Migration**

- Estimated effort: 2-3 days
- Use `redux-zustand-adapter` for gradual transition
- Low risk (both are JavaScript state)

**When to Consider:**

- Need advanced time-travel debugging
- Complex async flows (e.g., saga patterns)
- Team grows beyond 5 developers

---

## FAQ

### Q: Why not Context API?

**A:** Context API has re-render performance issues at scale. Every context update re-renders all consumers, even if they don't use the changed value. Zustand optimizes this with fine-grained selectors.

### Q: Why not MobX?

**A:** MobX uses mutable state (less TypeScript-friendly). Zustand uses immutable updates, which is safer for E2EE apps where state mutations could leak sensitive data.

### Q: Can I use Zustand with React Native?

**A:** Yes! Zustand works identically in React Native (Phase 3 mobile apps). Same stores, same code.

### Q: How do I clear all data on logout?

**A:** Use the centralized reset path so renderer state and main-process credentials are cleared together:

```typescript
const { nuclearReset } = await import('../services/resetService');
nuclearReset();
```

Normal user-initiated logout should call `userStore.logout()`, which requests main-process logout before invoking that reset.

### Q: How do I debug WebSocket → Store flow?

**A:**

1. Enable Redux DevTools extension
2. Open DevTools → Redux tab → Select "ChatStore"
3. Watch state changes as WebSocket messages arrive
4. Add breakpoints in `useWebSocket.ts` message handlers

---

## Performance Benchmarks

**Bundle Size:**

- Zustand: 45KB (minified)
- Redux Toolkit: 155KB + React-Redux 45KB = 200KB
- **Savings:** 155KB (77% smaller)

**Memory Usage:**

- Zustand: ~4MB (all stores loaded)
- Redux: ~12MB (estimated with DevTools + middleware)
- **Savings:** ~8MB

**Electron Startup Time:**

- With Zustand: ~1.2s
- With Redux (estimated): ~1.3s
- **Improvement:** ~100ms faster

---

## Related Documentation

- [Zustand Official Docs](https://github.com/pmndrs/zustand)
- [WebSocket Integration](../src/renderer/hooks/useWebSocket.ts)
- [Message Queue](../src/renderer/services/messageQueue.ts)
- [E2EE Service](../src/renderer/services/e2eeService.ts)

---

## Changelog

**2026-02-18:** Initial core-store documentation, DevTools and persistence middleware documented
**2026-03-03:** Updated the Phase 1A–1C inventory, removed dead reference to analysis doc
**2026-03-27:** Updated to Zustand 5.0.12, added the Phase 2A stores
**2026-04-09:** Added the Phase 2B stores
**2026-07-18:** Updated auth ownership and lifecycle semantics, replaced the point-in-time store total with the canonical Key Counts reference
**2026-08-30:** Updated concern-grouped store paths and documented the non-Zustand support-module exclusion

---

**Maintained by:** Mark (Backend) + Michael (Frontend)

**Next Review:** Before the v1.0.0 GA cut (milestone due 2027-01-01)
