# State Management Architecture

**Framework:** Zustand v5.0.12
**Last Updated:** 2026-04-09
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

| Store                           | Purpose                                                      | Persisted             | Phase |
| ------------------------------- | ------------------------------------------------------------ | --------------------- | ----- |
| **authStore**                   | JWT tokens, auth state                                       | ✅ refresh token only | 1A    |
| **chatStore**                   | Messages, typing, connection                                 | ❌ (privacy)          | 1A    |
| **serverStore**                 | Server list, active server                                   | ✅ active server ID   | 1A    |
| **userStore**                   | User profile, preferences                                    | ❌                    | 1A    |
| **channelStore**                | Channel list, active channel                                 | ✅ active channel ID  | 1B    |
| **connectionStore**             | WebSocket connection state + `wireViolationCount` (PR #1184) | ❌                    | 1B    |
| **memberStore**                 | Server members, roles                                        | ❌                    | 1B    |
| **layoutStore**                 | UI layout state (sidebar, panels)                            | ✅ partial            | 1B    |
| **settingsStore**               | App settings, theme, font scale                              | ✅                    | 1B    |
| **unreadStore**                 | Unread message counts                                        | ❌                    | 1B    |
| **inviteStore**                 | Server invites                                               | ❌                    | 1B    |
| **draftSettingsStore**          | Unsaved settings drafts                                      | ❌                    | 1B    |
| **ttsSettingsStore**            | Text-to-speech settings                                      | ✅                    | 1B    |
| **voiceStore**                  | Voice state, participants, quality tiers                     | ✅ device settings    | 1C    |
| **audioSettingsStore**          | Audio input/output device prefs                              | ✅                    | 1C    |
| **videoSettingsStore**          | Video device and codec prefs                                 | ✅                    | 1C    |
| **dmStore**                     | DM conversations, participants                               | ❌                    | 1C    |
| **friendStore**                 | Friend list, friend requests                                 | ❌                    | 1C    |
| **privacyStore**                | Privacy settings, friend codes                               | ❌                    | 1C    |
| **clientConfigStore**           | Server-provided client configuration                         | ❌                    | 2A    |
| **mfaChallengeStore**           | MFA challenge state during auth flows                        | ❌                    | 2A    |
| **notificationStore**           | Desktop notification preferences and queue                   | ✅                    | 2A    |
| **osPermissionStore**           | OS-level permission states (mic, camera, screen)             | ❌                    | 2A    |
| **permissionStore**             | RBAC permission state for current server/channel             | ❌                    | 2A    |
| **channelScrollStore**          | Per-channel scroll position tracking                         | ❌                    | 2B    |
| **draftMessageStore**           | Per-channel unsent message drafts                            | ❌                    | 2B    |
| **keyboardShortcutStore**       | Keyboard shortcut configuration and state                    | ✅                    | 2B    |
| **notificationNavigationStore** | Notification click navigation targets                        | ❌                    | 2B    |
| **savedGifsStore**              | User's saved/favourite GIFs (Klipy integration)              | ✅                    | 2B    |
| **settingsOverlayStore**        | Settings panel open/close and active tab state               | ❌                    | 2B    |

**Total: 41 Zustand stores** across Phases 1A–2B — see [internal] "Key Counts" for the authoritative count. The inventory above details 30 core stores; 11 added in later work are not yet broken out individually here — see `src/renderer/stores/` for the canonical set.

---

## Store Details

### 1. authStore

**Purpose:** Authentication tokens and session management

**Location:** `src/renderer/stores/authStore.ts`

**State:**

```typescript
interface AuthState {
  accessToken: string | null;
  refreshToken: string | null;
  setTokens: (access: string, refresh: string) => void;
  clearTokens: () => void;
}
```

**Persistence:**

- ✅ **Persists:** `refreshToken` only (30-day lifetime)
- ❌ **Does NOT persist:** `accessToken` (15-minute lifetime, security best practice)
- **Storage Key:** `concord-auth`
- **Privacy Rationale:** Access tokens are short-lived and should not persist across app restarts

**DevTools:** Enabled as "AuthStore"

**Usage:**

```typescript
import { useAuthStore } from '@/stores/authStore';

// In component
const accessToken = useAuthStore((state) => state.accessToken);
const setTokens = useAuthStore((state) => state.setTokens);

// Direct access (non-reactive)
const token = useAuthStore.getState().accessToken;
```

---

### 2. chatStore

**Purpose:** Real-time messages, typing indicators, WebSocket connection status

**Location:** `src/renderer/stores/chatStore.ts`

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
import { useChatStore } from '@/stores/chatStore';

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

**Location:** `src/renderer/stores/serverStore.ts`

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
import { useServerStore } from '@/stores/serverStore';

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

**Location:** `src/renderer/stores/userStore.ts`

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
import { useUserStore } from '@/stores/userStore';

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

**All stores** use Zustand's `devtools` middleware for debugging.

**Setup:**

```typescript
import { devtools } from 'zustand/middleware';

export const useChatStore = create<ChatState>()(
  devtools(
    (set, get) => ({
      /* store logic */
    }),
    { name: 'ChatStore' } // Shows up in Redux DevTools!
  )
);
```

**Usage:**

1. Install [Redux DevTools Extension](https://github.com/reduxjs/redux-devtools)
2. Open browser DevTools → Redux tab
3. See all Zustand stores (AuthStore, ChatStore, ServerStore, UserStore)

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

**authStore** and **serverStore** use Zustand's `persist` middleware.

**Setup:**

```typescript
import { persist, devtools } from 'zustand/middleware';

export const useAuthStore = create<AuthState>()(
  devtools(
    persist(
      (set) => ({
        /* store logic */
      }),
      {
        name: 'concord-auth', // localStorage key
        partialize: (state) => ({
          refreshToken: state.refreshToken, // Only persist refresh token
        }),
      }
    ),
    { name: 'AuthStore' }
  )
);
```

**Privacy Controls:**

- Use `partialize` to control EXACTLY what persists
- Example: authStore persists `refreshToken` but NOT `accessToken`
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

**Best (custom equality function for complex objects):**

```typescript
import { shallow } from 'zustand/shallow';

const { addMessage, updateMessage } = useChatStore(
  (state) => ({
    addMessage: state.addMessage,
    updateMessage: state.updateMessage,
  }),
  shallow
);
```

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
  devtools(
    (set, get) => ({
      /* ... */
    }),
    { name: 'ChatStore' }
  )
  // No persist middleware!
);
```

**Auth tokens - partial persistence:**

```typescript
// authStore - ONLY persist refresh token
partialize: (state) => ({
  refreshToken: state.refreshToken, // ✅ Persist
  // accessToken: NOT persisted ❌
});
```

**Logout - clear ALL stores:**

```typescript
// userStore.logout()
logout: async () => {
  // Clear all stores
  useUserStore.getState().clearUser();
  useAuthStore.getState().clearTokens();
  useServerStore.getState().clearServers();
  useChatStore.getState().reset();
};
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
// userStore calls authStore and serverStore
logout: async () => {
  set({ user: null });
  useAuthStore.getState().clearTokens(); // ✅ Cross-store access
  useServerStore.getState().clearServers();
};
```

**But avoid circular dependencies!**

---

## Testing

### Unit Testing Stores

**Example (authStore):**

```typescript
import { renderHook, act } from '@testing-library/react';
import { useAuthStore } from '@/stores/authStore';

describe('authStore', () => {
  beforeEach(() => {
    // Clear store before each test
    useAuthStore.getState().clearTokens();
  });

  it('should set tokens', () => {
    const { result } = renderHook(() => useAuthStore());

    act(() => {
      result.current.setTokens('access123', 'refresh456');
    });

    expect(result.current.accessToken).toBe('access123');
    expect(result.current.refreshToken).toBe('refresh456');
  });
});
```

**Tests deferred to Issue #26 (Frontend Component Tests)**

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

**A:** Call each store's clear method:

```typescript
useAuthStore.getState().clearTokens();
useChatStore.getState().reset();
useServerStore.getState().clearServers();
useUserStore.getState().clearUser();
```

Or use the `userStore.logout()` action which does this automatically.

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

- Zustand: ~4MB (all 41 stores loaded)
- Redux: ~12MB (estimated with DevTools + middleware)
- **Savings:** ~8MB

**Electron Startup Time:**

- With Zustand: ~1.2s
- With Redux (estimated): ~1.3s
- **Improvement:** ~100ms faster

---

## Related Documentation

- [Zustand Official Docs](https://github.com/pmndrs/zustand)
- [WebSocket Integration](../services/WEBSOCKET_README.md)
- [Message Queue](../services/messageQueue.ts)
- [E2EE Architecture](../utils/crypto.ts)

---

## Changelog

**2026-02-18:** Initial documentation, 4 core stores documented, DevTools + persist middleware enabled
**2026-03-03:** Updated store inventory to 19 stores (Phase 1A–1C complete), removed dead reference to analysis doc
**2026-03-27:** Updated to Zustand 5.0.12, added 5 Phase 2A stores (clientConfigStore, mfaChallengeStore, notificationStore, osPermissionStore, permissionStore), total now 24
**2026-04-09:** Added 6 Phase 2B stores (channelScrollStore, draftMessageStore, keyboardShortcutStore, notificationNavigationStore, savedGifsStore, settingsOverlayStore), total now 30

---

**Maintained by:** Mark (Backend) + Michael (Frontend)

<!-- audit-exempt: historical reference (v0.1.0-Alpha is a shipped versioned identifier) -->

**Next Review:** Before v0.1.0-Alpha release
