/**
 * WebSocket Service - Connection Manager for Concord Real-Time Communication
 *
 * Features:
 * - JWT authentication
 * - Auto-reconnect with exponential backoff
 * - Heartbeat/ping-pong handling
 * - Connection state management
 * - Message routing with type-based handlers
 * - Subscribe/unsubscribe to channels
 * - Typing indicators
 *
 * @see services/control-plane/internal/websocket for backend implementation
 */

import { WS_BASE } from '../config';
import { useAuthStore } from '../stores/authStore';
import { useConnectionStore } from '../stores/connectionStore';
import { errorMessage, errorName } from '../utils/redactError';
import { summarizeWsDiagnostic, summarizeWsServerError } from '../utils/wsDiagnostics';
import {
  WebSocketEventSchema,
  scrubZodIssues,
  type WebSocketEvent,
  type WSEventType,
} from '../types/ws-events';

// ─── Connection-boundary validation (defense-in-depth) ────────────────────
//
// The WebSocket connection target (`this.url`) and the auth ticket flow into
// the URL passed to `new WebSocket(wsUrl)`. `encodeURIComponent(ticket)`
// already mitigates URL injection on the ticket, but a future regression
// that drops the encode or lets `this.url` become attacker-influenced could
// escalate to arbitrary-host redirection. These boundary checks fail-closed
// before the WebSocket constructor sees the URL.

/**
 * Ticket allowlist: alphanumeric plus `._-` only.
 * Accepts the current hex format (`hex.EncodeToString` server-side) and any
 * future JWT (`header.payload.signature` base64url-with-dots) without admitting
 * URL-control characters (`?`, `&`, `=`, `#`, `/`, etc.) that could escape the
 * `?ticket=` query parameter into adjacent URL components.
 */
const TICKET_RE = /^[A-Za-z0-9._-]+$/;
/** Generous cap that admits real JWTs while rejecting megabyte-scale payloads. */
const MAX_TICKET_LEN = 4096;

function isValidWsTicket(t: unknown): t is string {
  return typeof t === 'string' && t.length > 0 && t.length <= MAX_TICKET_LEN && TICKET_RE.test(t);
}

/**
 * Validate the configured WebSocket base URL before connection.
 * Returns the parsed URL on success, or null on rejection.
 * Production builds (`import.meta.env.PROD`) require `wss:` specifically;
 * dev/test accept `ws:` for localhost.
 */
function validateWsBaseUrl(rawUrl: string): URL | null {
  let url: URL;
  try {
    url = new URL(rawUrl);
  } catch {
    return null;
  }
  if (url.protocol !== 'wss:' && url.protocol !== 'ws:') return null;
  if (import.meta.env.PROD && url.protocol !== 'wss:') return null;
  return url;
}

export enum ConnectionState {
  DISCONNECTED = 'disconnected',
  CONNECTING = 'connecting',
  CONNECTED = 'connected',
  RECONNECTING = 'reconnecting',
  ERROR = 'error',
}

/**
 * Outgoing command surface — what the client SENDS to the server. Loose-typed
 * for now; typing this side with a discriminated union (symmetric to incoming
 * WebSocketEvent) is out of scope for #709 and tracked as a follow-up issue.
 */
interface OutgoingWebSocketCommand {
  type: string;
  data: Record<string, unknown>;
}

export interface ConnectionInfo {
  clientId: string;
  userId: string;
}

type MessageHandler = (message: WebSocketEvent) => void;
type ConnectionHandler = (state: ConnectionState) => void;

/**
 * Full-jitter delay: random value in [0, base).
 * Chosen over equal-jitter because reconnect storms benefit more from
 * desynchronisation than from preserving a minimum delay.
 * See AWS Architecture Blog, "Exponential Backoff and Jitter".
 */
export function fullJitter(base: number): number {
  if (base <= 0) return 0;
  return Math.floor(Math.random() * base);
}

export class WebSocketService {
  private ws: WebSocket | null = null;
  private readonly url: string;
  private token: string | null = null;
  private state: ConnectionState = ConnectionState.DISCONNECTED;

  // Connection management
  private reconnectAttempts = 0;
  private readonly maxReconnectAttempts = 10;
  private readonly MAX_AGGRESSIVE_ATTEMPTS = 3;
  private ticketCache: { ticket: string; issuedAt: number } | null = null;
  private readonly TICKET_CACHE_TTL_MS = 5_000;
  private readonly CONNECTION_READY_TIMEOUT_MS = 5_000;
  private connectionReadyPromise: Promise<void> | null = null;
  private connectionReadyResolve: (() => void) | null = null;
  private connectionReadyReject: ((err: Error) => void) | null = null;
  private connectionReadyTimer: NodeJS.Timeout | null = null;
  private reconnectDelay = 1000; // Start with 1 second
  private readonly maxReconnectDelay = 30000; // Max 30 seconds
  private reconnectTimer: NodeJS.Timeout | null = null;
  private pingInterval: NodeJS.Timeout | null = null;
  private aggressiveReconnect = false;
  private connectAbort: AbortController | null = null;
  // 'online' event listener armed while a connect is deferred because the
  // client is known-offline (navigator.onLine === false). Cleared on the
  // open/disconnect paths and when it fires.
  private onlineListener: (() => void) | null = null;
  // Bounded fallback timer armed alongside onlineListener. navigator.onLine is
  // NOT reliable in Electron — it can stick `false` and the 'online' event may
  // never (re)fire (e.g. across a server-deploy network flap). If 'online'
  // hasn't fired within ONLINE_FALLBACK_MS, force a real reconnect attempt
  // rather than wait forever, which would strand the client in RECONNECTING
  // until a manual app restart (regression #1768).
  private onlineFallbackTimer: NodeJS.Timeout | null = null;
  private readonly ONLINE_FALLBACK_MS = 15_000;

  // Message handlers
  private readonly messageHandlers = new Map<string, Set<MessageHandler>>();
  private readonly connectionHandlers = new Set<ConnectionHandler>();

  // Connection info
  private connectionInfo: ConnectionInfo | null = null;

  // Subscribed channels, servers, and DM conversations (tracked for resubscription on reconnect)
  private readonly subscribedChannels = new Set<string>();
  private readonly subscribedServers = new Set<string>();
  private readonly subscribedDMs = new Set<string>();

  constructor(baseUrl: string = WS_BASE) {
    this.url = `${baseUrl}/api/v1/ws`;
  }

  /**
   * Connect to WebSocket server with JWT token
   */
  connect(token: string): void {
    if (this.state === ConnectionState.CONNECTED || this.state === ConnectionState.CONNECTING) {
      return;
    }

    this.token = token;
    this.connectAbort?.abort();
    this.connectAbort = new AbortController();
    this.setState(ConnectionState.CONNECTING);
    this.createConnection(this.connectAbort.signal);
  }

  /**
   * Disconnect from WebSocket server
   */
  disconnect(): void {
    this.connectAbort?.abort();
    this.connectAbort = null;
    this.clearReconnectTimer();
    this.clearPingInterval();
    this.clearOnlineRetry();
    this.reconnectAttempts = 0;

    // Reject any in-flight whenConnectionReady() waiters with a clear reason.
    // handleClose() does the same on involuntary close; a client-initiated
    // disconnect (logout, token cleared) also needs to settle the promise so
    // consumers don't sit on a stale promise that only rejects via the 5s
    // connection-ready timeout with a misleading "timeout" error.
    this.rejectConnectionReady(new Error('disconnected by client'));

    if (this.ws) {
      // Detach all handlers first to prevent zombie sockets from firing events
      // (e.g. a CONNECTING socket whose onopen fires after we've created a new one)
      this.ws.onopen = null;
      this.ws.onmessage = null;
      this.ws.onerror = null;
      this.ws.onclose = null;

      // Only close OPEN sockets. CONNECTING sockets will silently open and
      // get GC'd since we've already detached all handlers above.
      // Calling close() on CONNECTING triggers a noisy browser warning.
      if (this.ws.readyState === WebSocket.OPEN) {
        this.ws.close(1000, 'Client disconnect');
      }
      this.ws = null;
    }

    this.setState(ConnectionState.DISCONNECTED);
    this.connectionInfo = null;
  }

  /**
   * Get current connection state
   */
  getState(): ConnectionState {
    return this.state;
  }

  /**
   * Get connection info (client ID and user ID)
   */
  getConnectionInfo(): ConnectionInfo | null {
    return this.connectionInfo;
  }

  /**
   * Reset reconnect backoff state to defaults.
   * Used when the caller wants to ensure a fresh backoff curve on the next
   * connect attempt (e.g., after a successful token refresh + manual retry).
   */
  resetReconnectState(): void {
    this.reconnectAttempts = 0;
    this.reconnectDelay = 1000;
    this.clearReconnectTimer();
  }

  /**
   * Update the stored JWT token without a full disconnect/connect cycle.
   * Used when the main process refreshes the token proactively or during
   * manual retry so the next ws-ticket fetch uses the fresh token.
   */
  updateToken(token: string): void {
    this.token = token;
  }

  /**
   * Toggle aggressive reconnection mode.
   * When enabled, reconnect attempts use rapid intervals (500ms → 2s)
   * instead of the normal exponential backoff (1s → 30s).
   * Used during the 15-second grace period after connection loss.
   */
  setAggressiveReconnect(enabled: boolean): void {
    this.aggressiveReconnect = enabled;
    // If switching to aggressive while a slow reconnect is pending, re-schedule faster
    if (enabled && this.reconnectTimer) {
      this.clearReconnectTimer();
      this.scheduleReconnect();
    }
  }

  /**
   * Send an outgoing command to the WebSocket server.
   *
   * Outgoing commands currently use OutgoingWebSocketCommand's permissive
   * shape. The stricter discriminated-union follow-up is tracked separately
   * from #709.
   */
  send(message: OutgoingWebSocketCommand): void {
    if (this.ws?.readyState !== WebSocket.OPEN) {
      // Subscription lifecycle messages are expected when WS isn't open (startup, teardown)
      const subTypes = ['subscribe', 'subscribe_server', 'unsubscribe', 'unsubscribe_server'];
      if (!subTypes.includes(message.type)) {
        console.warn('Cannot send message: WebSocket not open', message.type);
      }
      return;
    }

    try {
      this.ws.send(JSON.stringify(message));
    } catch (error) {
      console.error('Failed to send WebSocket message:', errorMessage(error));
    }
  }

  /**
   * Check if a channel is subscribed
   */
  isSubscribed(channelId: string): boolean {
    return this.subscribedChannels.has(channelId);
  }

  /**
   * Resolves when the hub has acknowledged all resubscribes on the current
   * connection via connection_ready. Rejects on disconnect or 5s timeout.
   * Consumers (e.g., message queue) should gate work on this during reconnect.
   */
  whenConnectionReady(): Promise<void> {
    return this.connectionReadyPromise ?? Promise.resolve();
  }

  /**
   * Subscribe to a channel
   */
  subscribe(channelId: string): void {
    this.subscribedChannels.add(channelId);
    this.send({
      type: 'subscribe',
      data: { channel_id: channelId },
    });
  }

  /**
   * Unsubscribe from a channel
   */
  unsubscribe(channelId: string): void {
    this.subscribedChannels.delete(channelId);
    this.send({
      type: 'unsubscribe',
      data: { channel_id: channelId },
    });
  }

  /**
   * Subscribe to server-level notifications (lightweight unread pings)
   */
  subscribeServer(serverId: string): void {
    this.subscribedServers.add(serverId);
    this.send({
      type: 'subscribe_server',
      data: { server_id: serverId },
    });
  }

  /**
   * Unsubscribe from server-level notifications
   */
  unsubscribeServer(serverId: string): void {
    this.subscribedServers.delete(serverId);
    this.send({
      type: 'unsubscribe_server',
      data: { server_id: serverId },
    });
  }

  /**
   * Send a chat message to a channel
   */
  sendMessage(
    channelId: string,
    content: string,
    opts?: {
      nonce?: string;
      keyVersion?: number;
      mentionMeta?: string;
      replyToId?: string;
      attachmentIds?: string[];
      gifSlug?: string;
    }
  ): void {
    const { nonce, keyVersion, mentionMeta, replyToId, attachmentIds, gifSlug } = opts ?? {};
    const data: Record<string, unknown> = {
      channel_id: channelId,
      content,
      key_version: keyVersion,
      nonce,
      timestamp: Date.now(),
    };
    if (mentionMeta) {
      data.mention_meta = mentionMeta;
    }
    if (replyToId) {
      data.reply_to_id = replyToId;
    }
    if (attachmentIds && attachmentIds.length > 0) {
      data.attachment_ids = attachmentIds;
    }
    if (gifSlug) {
      data.gif_slug = gifSlug;
    }
    this.send({ type: 'message', data });
  }

  /**
   * Notify the server that the user's profile has been updated
   */
  sendProfileUpdate(): void {
    this.send({
      type: 'profile_update',
      data: {},
    });
  }

  /**
   * Notify the server that a server's settings have been updated
   */
  sendServerUpdate(serverId: string): void {
    this.send({
      type: 'server_update',
      data: { server_id: serverId },
    });
  }

  /**
   * Send typing indicator
   */
  sendTypingIndicator(channelId: string, isTyping: boolean): void {
    this.send({
      type: 'typing',
      data: {
        channel_id: channelId,
        is_typing: isTyping,
      },
    });
  }

  /**
   * Subscribe to a DM conversation
   */
  subscribeDM(conversationId: string): void {
    this.subscribedDMs.add(conversationId);
    this.send({
      type: 'subscribe_dm',
      data: { conversation_id: conversationId },
    });
  }

  /**
   * Unsubscribe from a DM conversation
   */
  unsubscribeDM(conversationId: string): void {
    this.subscribedDMs.delete(conversationId);
    this.send({
      type: 'unsubscribe_dm',
      data: { conversation_id: conversationId },
    });
  }

  /**
   * Check if a DM conversation is subscribed
   */
  isDMSubscribed(conversationId: string): boolean {
    return this.subscribedDMs.has(conversationId);
  }

  /**
   * Send a DM message
   */
  sendDMMessage(
    conversationId: string,
    content: string,
    opts: {
      nonce?: string;
      keyVersion?: number;
      mentionMeta?: string;
      attachmentIds?: string[];
      replyToId?: string;
      gifSlug?: string;
    } = {}
  ): void {
    const data: Record<string, unknown> = {
      conversation_id: conversationId,
      content,
      key_version: opts.keyVersion,
      nonce: opts.nonce,
      timestamp: Date.now(),
    };
    if (opts.mentionMeta) {
      data.mention_meta = opts.mentionMeta;
    }
    if (opts.attachmentIds && opts.attachmentIds.length > 0) {
      data.attachment_ids = opts.attachmentIds;
    }
    if (opts.replyToId) {
      data.reply_to_id = opts.replyToId;
    }
    if (opts.gifSlug) {
      data.gif_slug = opts.gifSlug;
    }
    this.send({ type: 'dm_message', data });
  }

  /**
   * Send DM typing indicator
   */
  sendDMTypingIndicator(conversationId: string, isTyping: boolean): void {
    this.send({
      type: 'dm_typing',
      data: {
        conversation_id: conversationId,
        is_typing: isTyping,
      },
    });
  }

  /**
   * Send heartbeat to refresh Redis presence TTL
   */
  sendHeartbeat(): void {
    this.send({
      type: 'heartbeat',
      data: {},
    });
  }

  /**
   * Send status change (online/dnd/invisible)
   */
  sendSetStatus(status: 'online' | 'dnd' | 'invisible'): void {
    this.send({
      type: 'set_status',
      data: { status },
    });
  }

  /**
   * Add a message handler for a specific event type.
   *
   * The generic `T` is inferred from the `messageType` literal at the call
   * site, and TypeScript narrows the handler's `event` parameter to the
   * matching schema's payload via `Extract<WebSocketEvent, { type: T }>`.
   *
   * Runtime validation happens BEFORE handler dispatch (see handleMessage).
   * Handlers can rely on event.data conforming to the schema — no defensive
   * `if (!field)` checks needed for schema-required fields.
   *
   * @example
   *   wsService.on('dm_message', (event) => {
   *     // event.data is typed as DMMessagePayload — no cast required
   *     useDMStore.getState().receive(event.data.conversation_id, event.data.content);
   *   });
   */
  on<T extends WSEventType>(
    messageType: T,
    handler: (event: Extract<WebSocketEvent, { type: T }>) => void
  ): () => void {
    let handlers = this.messageHandlers.get(messageType);
    if (!handlers) {
      handlers = new Set();
      this.messageHandlers.set(messageType, handlers);
    }

    // The handler's parameter type is narrower than MessageHandler — the
    // internal Map stores broader types and we narrow at dispatch via the
    // type discriminator. Cast is safe because handleMessage only invokes a
    // handler with an event whose `type` matches the handler's registration
    // key (post-safeParse).
    handlers.add(handler as MessageHandler);

    // Return unsubscribe function
    return () => {
      const handlers = this.messageHandlers.get(messageType);
      if (handlers) {
        handlers.delete(handler as MessageHandler);
        if (handlers.size === 0) {
          this.messageHandlers.delete(messageType);
        }
      }
    };
  }

  /**
   * Add a connection state change handler
   */
  onConnectionChange(handler: ConnectionHandler): () => void {
    this.connectionHandlers.add(handler);

    // Immediately call handler with current state
    handler(this.state);

    // Return unsubscribe function
    return () => {
      this.connectionHandlers.delete(handler);
    };
  }

  /**
   * Remove all handlers (cleanup)
   */
  removeAllHandlers(): void {
    this.messageHandlers.clear();
    this.connectionHandlers.clear();
  }

  // Private methods

  /**
   * Consume a cached ticket if present and within TTL.
   * Single-use: always clears the cache entry, even if expired.
   * Returns null if no cache hit.
   */
  private consumeCachedTicket(): string | null {
    const entry = this.ticketCache;
    this.ticketCache = null;
    if (!entry) return null;
    if (Date.now() - entry.issuedAt >= this.TICKET_CACHE_TTL_MS) return null;
    return entry.ticket;
  }

  /**
   * Log a specific warning for 401 ticket-fetch failures. Extracted to keep
   * createConnection's cognitive complexity below the 15-point ceiling.
   *
   * 401 means the token used for this ticket request was rejected — typically
   * because a JWT rotation landed between the time scheduleReconnect armed
   * the retry and the time the ticket fetch actually dispatched. updateToken()
   * has refreshed this.token by now; the next scheduleReconnect cycle will
   * use the fresh value. The warn surfaces this transient mode so the
   * generic "Failed to create WebSocket connection" log isn't the only
   * signal a developer sees.
   */
  private logTicketFetchFailure(status: number): void {
    if (status === 401) {
      console.warn(
        '[WebSocket] ticket fetch returned 401 — token likely rotated mid-connect; will retry'
      );
    }
  }

  /**
   * Obtain a single-use WebSocket ticket: reuse a cached ticket within TTL,
   * otherwise POST to the ws-ticket endpoint with the Bearer token. Throws on a
   * non-OK response. Extracted from createConnection to keep that method's
   * cognitive complexity under the S3776 ceiling (the offline-gate branch tipped
   * it over). Caller must guarantee this.token is non-null before invoking.
   */
  private async acquireTicket(wsBaseUrl: URL, signal?: AbortSignal): Promise<string> {
    const cached = this.consumeCachedTicket();
    if (cached) return cached;

    // Derive the ticket endpoint from the validated URL via URL-object mutations
    // rather than string `replace()` — protocol/pathname swaps through the WHATWG
    // URL API can't slip through a malformed-substring bug if `this.url` ever
    // shifts shape.
    const ticketUrl = new URL(wsBaseUrl.href);
    ticketUrl.protocol = wsBaseUrl.protocol === 'wss:' ? 'https:' : 'http:';
    ticketUrl.pathname = ticketUrl.pathname.replace(/\/ws$/, '/auth/ws-ticket');

    const ticketHeaders: Record<string, string> = { Authorization: `Bearer ${this.token}` };
    const sessionId = useAuthStore.getState().sessionId;
    if (sessionId) ticketHeaders['X-Session-ID'] = sessionId;

    const ticketRes = await fetch(ticketUrl.href, {
      method: 'POST',
      headers: ticketHeaders,
      signal,
    });
    if (!ticketRes.ok) {
      this.logTicketFetchFailure(ticketRes.status);
      throw new Error(`WebSocket ticket request failed: ${ticketRes.status}`);
    }
    return (await ticketRes.json()).ticket as string;
  }

  private async createConnection(signal?: AbortSignal, forceDespiteOffline = false): Promise<void> {
    if (!this.token) {
      console.error('Cannot connect: No JWT token provided');
      this.setState(ConnectionState.ERROR);
      return;
    }

    // Validate the WebSocket base URL FIRST — before any network call — so a
    // tainted `this.url` cannot exfiltrate the Bearer token to an attacker-
    // controlled host via the ticket fetch. Per Copilot review on PR #943:
    // if validation runs after the ticket request, the Authorization header
    // is already on the wire before the scheme check fails.
    //
    // URL-validation failure is treated as a TERMINAL config error — we do
    // not call scheduleReconnect() because retrying the same bad URL would
    // just retry the leak attempt. The operator must fix the configured URL.
    const wsBaseUrl = validateWsBaseUrl(this.url);
    if (!wsBaseUrl) {
      console.error('[WebSocket] refused to connect: URL failed scheme validation');
      this.setState(ConnectionState.ERROR);
      return;
    }

    // Known-offline short-circuit: skip the ws-ticket POST that is guaranteed to
    // fail (it would just emit a TypeError and burn a backoff cycle) and wait
    // for the browser 'online' event instead. navigator.onLine === false is a
    // reliable "definitely offline" signal; navigator.onLine === true is NOT
    // reliable (it only means a route exists), which is why this deliberately
    // does nothing for the origin-502 case — there the client IS online.
    if (this.isKnownOffline() && !forceDespiteOffline) {
      console.debug('[WebSocket] offline — deferring connect until the online event');
      this.setState(ConnectionState.RECONNECTING);
      this.armOnlineRetry();
      return;
    }

    try {
      const ticket = await this.acquireTicket(wsBaseUrl, signal);

      // If disconnect() was called while we awaited the ticket, cache it for reuse
      if (signal?.aborted) {
        this.ticketCache = { ticket, issuedAt: Date.now() };
        return;
      }

      if (!isValidWsTicket(ticket)) {
        // Ticket-shape failure is treated as TRANSIENT — a server bug or
        // race could yield a malformed ticket. Schedule a reconnect so the
        // client recovers automatically rather than parking in ERROR forever.
        console.error('[WebSocket] refused to connect: ticket failed shape validation');
        this.setState(ConnectionState.ERROR);
        this.scheduleReconnect();
        return;
      }

      // Build the final WebSocket URL from the validated base. URL.searchParams.set
      // handles percent-encoding automatically — equivalent to encodeURIComponent
      // but safer because it can't be undone by a future regression that drops
      // the encode call.
      const finalUrl = new URL(wsBaseUrl.href);
      finalUrl.searchParams.set('ticket', ticket);

      this.ws = new WebSocket(finalUrl.href);

      this.ws.onopen = this.handleOpen.bind(this);
      this.ws.onmessage = this.handleMessage.bind(this);
      this.ws.onerror = this.handleError.bind(this);
      this.ws.onclose = this.handleClose.bind(this);
    } catch (error) {
      // AbortError is expected when disconnect() cancels an in-flight ticket fetch
      if (error instanceof DOMException && error.name === 'AbortError') return;
      // Log error.name (e.g. "SyntaxError"), NOT the raw error or its message.
      // `new WebSocket(url)` throws synchronously with a SyntaxError whose
      // message includes the full URL — including the `?ticket=<hex>` query
      // param. Logging .name preserves triage signal without leaking the
      // single-use auth bearer.
      console.error('[WebSocket] failed to create connection:', errorName(error));
      this.setState(ConnectionState.ERROR);
      this.scheduleReconnect();
    }
  }

  private handleOpen(): void {
    console.debug('[WebSocket] connection opened');
    this.clearOnlineRetry();
    // Don't set CONNECTED yet — wait for the server's "connected" message
    // which includes client_id and user_id. This prevents subscribers from
    // trying to send before the handshake is complete.
    this.reconnectAttempts = 0;
    this.reconnectDelay = 1000;

    // Start ping interval to detect connection health
    this.startPingInterval();
  }

  private handleMessage(event: MessageEvent): void {
    // ── 1. JSON parse (transport-level failure: log + drop, no counter) ────
    let raw: unknown;
    try {
      raw = JSON.parse(event.data);
    } catch (error) {
      console.error('Failed to parse WebSocket message:', errorMessage(error));
      return;
    }

    // ── 2. Schema validation at the trust boundary ────────────────────────
    // PII-scrubbing invariant: the log call MUST use scrubZodIssues() so the
    // rejected payload's `received` field (which can carry PII — DM content,
    // usernames, avatar URLs) does NOT flow into the log sink.
    // See [internal]rules/observability.md and the sentinel-string test in
    // tests/unit/services/websocketService.dispatch.test.ts.
    const parsed = WebSocketEventSchema.safeParse(raw);
    if (!parsed.success) {
      const rawType =
        typeof raw === 'object' && raw !== null && 'type' in raw
          ? String((raw as { type?: unknown }).type)
          : '<unknown>';
      // Discriminator-mismatch produces issues[0].code === 'invalid_union' in
      // zod 4.x (the pre-4 name was 'invalid_union_discriminator'). When the
      // discriminator value isn't in the union's literal list, this is the
      // "unknown event type" case — client may be outdated relative to server.
      const isUnknownType = parsed.error.issues.some((i) => i.code === 'invalid_union');
      // Format-string injection defense (CWE-134): keep the prefix string a
      // constant literal; never interpolate the server-supplied `rawType` into
      // the format string itself. console.error treats argument 1 as a printf-
      // like format string, so a `rawType` containing `%s`/`%d` would consume
      // the second argument unexpectedly. The structured object below carries
      // `rawType` as a typed field, where it's safe.
      const logPrefix = isUnknownType
        ? '[WS] unknown event type — client may be outdated'
        : '[WS] wire violation';
      // Structured metadata only — issues are PII-scrubbed via scrubZodIssues. The last
      // console.error arg here is an object literal (not a bare Error identifier), so the
      // no-restricted-syntax raw-err guard's selector doesn't match.
      console.error(logPrefix, {
        type: rawType,
        issues: scrubZodIssues(parsed.error.issues),
      });
      useConnectionStore.getState().incrementWireViolation();
      return;
    }
    const message: WebSocketEvent = parsed.data;

    // ── 3. Internal handling for envelope-level event types ───────────────
    // These are consumed by wsService itself, not by wsService.on() subscribers.
    // The schema-narrowed `message.data.<field>` accesses are now type-safe
    // (no `as string` casts) because ConnectedSchema / ConnectionReadySchema
    // are in WebSocketEventSchema.
    switch (message.type) {
      case 'connected':
        this.connectionInfo = {
          clientId: message.data.client_id,
          userId: message.data.user_id,
        };
        // Reset the wire-violation counter at the start of each fresh
        // connection. Per-connection reset (not cumulative across reconnects)
        // produces an actionable session-level drift signal, not one inflated
        // by reconnect storms during network outages.
        useConnectionStore.setState({ wireViolationCount: 0 });
        console.debug('[WebSocket] connected:', this.connectionInfo);

        // Create a fresh connection-ready promise BEFORE sending the probe
        // so consumers that subscribe now observe the unresolved state.
        this.createConnectionReadyPromise();

        // Resubscribe to channels after reconnect (task 5 adds probe emission here)
        this.resubscribeChannels();

        // Now that handshake is complete, mark as connected
        this.setState(ConnectionState.CONNECTED);
        break;

      case 'error':
        // message.data is an ErrorSchema-validated server payload object (not a JS Error).
        // The last console.error arg is a member-expression, not a bare Error identifier,
        // so the no-restricted-syntax raw-err guard's selector doesn't match.
        console.error('WebSocket server error:', summarizeWsServerError(message.data));
        break;

      case 'connection_ready':
        this.resolveConnectionReady();
        break;
    }

    // ── 4. Dispatch to subscriber handlers ────────────────────────────────
    // Per-handler try/catch isolates exceptions — one buggy handler can't
    // break others for the same event type. Validation failures (above)
    // never reach this loop, so handlers can rely on schema-guaranteed shape.
    const handlers = this.messageHandlers.get(message.type);
    if (handlers) {
      for (const handler of handlers) {
        try {
          handler(message);
        } catch (error) {
          console.error('Error in message handler:', errorMessage(error));
        }
      }
    }
  }

  private handleError(event: Event): void {
    // Log `event.type` (the string "error") rather than the raw `event` —
    // serializing the Event would leak `event.target.url`, which contains
    // the single-use auth `?ticket=<hex>` query param.
    //
    // Downgrade to warn during the first-attempt aggressive bounce: the
    // 1006-then-retry pattern is the expected manifestation of the CF-edge
    // upgrade flakiness already mitigated by the aggressive-backoff
    // infrastructure (#769 / origin-502-storm). Surfacing as error every
    // cold start is log noise that masks real failures. Once aggressive
    // mode has burned through an attempt (or is off), restore the loud
    // signal — that's when something is actually wrong.
    const isExpectedFirstBounce = this.reconnectAttempts === 0 && this.aggressiveReconnect;
    const summary = summarizeWsDiagnostic(event);
    if (isExpectedFirstBounce) {
      // eslint-disable-next-line no-restricted-syntax -- summary is an audit-safe structured diagnostic; raw Event/Error objects and cause chains are not logged.
      console.warn('[WebSocket] first-attempt transport drop (will retry):', summary);
    } else {
      // eslint-disable-next-line no-restricted-syntax -- summary is an audit-safe structured diagnostic; raw Event/Error objects and cause chains are not logged.
      console.error('[WebSocket] transport error:', summary);
    }
    this.setState(ConnectionState.ERROR);
  }

  private handleClose(event: CloseEvent): void {
    console.debug(`[WebSocket] closed (code: ${event.code}, reason: ${event.reason || 'none'})`);

    this.clearPingInterval();
    this.ws = null;

    // If a connection-ready wait was in flight, reject it so waiters see the failure
    this.rejectConnectionReady(new Error('disconnected before connection_ready'));

    // Only reconnect if not manually disconnected
    if (event.code === 1000) {
      this.setState(ConnectionState.DISCONNECTED);
    } else {
      this.setState(ConnectionState.RECONNECTING);
      this.scheduleReconnect();
    }
  }

  private scheduleReconnect(): void {
    this.clearReconnectTimer();

    // Aggressive cap: after MAX_AGGRESSIVE_ATTEMPTS, drop to normal exponential.
    if (this.aggressiveReconnect && this.reconnectAttempts >= this.MAX_AGGRESSIVE_ATTEMPTS) {
      this.aggressiveReconnect = false;
      this.reconnectAttempts = 0;
    }

    let delay: number;
    if (this.aggressiveReconnect) {
      // Aggressive with full jitter: 500ms → 1s → 2s base, each scaled by random [0,1)
      const base = Math.min(500 * Math.pow(2, Math.min(this.reconnectAttempts, 2)), 2000);
      delay = fullJitter(base);
    } else {
      // Normal: exponential backoff for the first maxReconnectAttempts, then a
      // fixed 30s ceiling (keeps retrying indefinitely — the give-up UX lives in
      // useConnectionRecovery's preflight/recovery_a/fatal phases, not here).
      // Full jitter is applied to the computed base — the SAME desync the
      // aggressive branch uses above — so a fleet-wide origin outage doesn't
      // resolve into a synchronized thundering herd reconnecting in lockstep at
      // the 1s/2s/.../30s boundaries against a just-recovered origin
      // (#769 / origin-502-storm). Without it, every client that dropped at the
      // same instant retries at the same instant.
      const base =
        this.reconnectAttempts < this.maxReconnectAttempts
          ? Math.min(
              this.reconnectDelay * Math.pow(2, this.reconnectAttempts),
              this.maxReconnectDelay
            )
          : this.maxReconnectDelay;
      delay = fullJitter(base);
    }

    console.debug(
      `[WebSocket] reconnecting in ${delay}ms (attempt ${this.reconnectAttempts + 1}${this.aggressiveReconnect ? ', aggressive' : ''})`
    );

    this.reconnectTimer = setTimeout(() => {
      this.reconnectAttempts++;
      this.connectAbort?.abort();
      this.connectAbort = new AbortController();
      this.createConnection(this.connectAbort.signal);
    }, delay);
  }

  private clearReconnectTimer(): void {
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  /** Reliable "definitely offline" check — false when navigator is unavailable. */
  private isKnownOffline(): boolean {
    return typeof navigator !== 'undefined' && navigator.onLine === false;
  }

  /**
   * Arm a one-shot 'online' listener that retries the connection (with a fresh
   * backoff curve) as soon as connectivity returns. Idempotent. Falls back to
   * the normal backoff loop in a non-browser environment with no event target.
   */
  private armOnlineRetry(): void {
    if (this.onlineListener) return;
    if (typeof globalThis.addEventListener !== 'function') {
      this.scheduleReconnect();
      return;
    }
    // Shared by both the 'online' fast-path and the bounded fallback timer: do a
    // real reconnect attempt that BYPASSES the offline gate (forceDespiteOffline)
    // — otherwise the retry would re-hit isKnownOffline() and re-defer forever.
    const reconnectNow = () => {
      this.clearOnlineRetry();
      this.resetReconnectState();
      this.connectAbort?.abort();
      this.connectAbort = new AbortController();
      this.createConnection(this.connectAbort.signal, true);
    };
    const listener = () => reconnectNow();
    this.onlineListener = listener;
    globalThis.addEventListener('online', listener);
    // Safety net for a stuck navigator.onLine where 'online' never fires (#1768).
    this.onlineFallbackTimer = setTimeout(reconnectNow, this.ONLINE_FALLBACK_MS);
  }

  private clearOnlineRetry(): void {
    if (this.onlineListener) {
      globalThis.removeEventListener('online', this.onlineListener);
      this.onlineListener = null;
    }
    if (this.onlineFallbackTimer) {
      clearTimeout(this.onlineFallbackTimer);
      this.onlineFallbackTimer = null;
    }
  }

  private startPingInterval(): void {
    this.clearPingInterval();

    // Send heartbeat every 30 seconds to refresh Redis presence TTL (120s)
    this.pingInterval = setInterval(() => {
      if (this.state === ConnectionState.CONNECTED && this.ws) {
        this.sendHeartbeat();
      }
    }, 30000);
  }

  private clearPingInterval(): void {
    if (this.pingInterval) {
      clearInterval(this.pingInterval);
      this.pingInterval = null;
    }
  }

  private createConnectionReadyPromise(): void {
    // Clear any prior unresolved promise state
    if (this.connectionReadyTimer) {
      clearTimeout(this.connectionReadyTimer);
      this.connectionReadyTimer = null;
    }
    this.connectionReadyPromise = new Promise<void>((resolve, reject) => {
      this.connectionReadyResolve = resolve;
      this.connectionReadyReject = reject;
    });
    // Pre-attach a no-op catch so the promise is never "unhandled" when no
    // consumer awaits it. Consumers that DO await whenConnectionReady() still
    // see the rejection via their own .then()/.catch() chain — Promise
    // rejections propagate to all attached handlers, not just the first.
    this.connectionReadyPromise.catch(() => {});
    this.connectionReadyTimer = setTimeout(() => {
      // Funnel through rejectConnectionReady so timer + resolve/reject refs are
      // cleared consistently (avoids stale-ref state after timeout).
      this.rejectConnectionReady(
        new Error('connection_ready timeout after 5s — proceeding best-effort')
      );
    }, this.CONNECTION_READY_TIMEOUT_MS);
  }

  private resolveConnectionReady(): void {
    if (this.connectionReadyTimer) {
      clearTimeout(this.connectionReadyTimer);
      this.connectionReadyTimer = null;
    }
    this.connectionReadyResolve?.();
    this.connectionReadyResolve = null;
    this.connectionReadyReject = null;
  }

  private rejectConnectionReady(err: Error): void {
    if (this.connectionReadyTimer) {
      clearTimeout(this.connectionReadyTimer);
      this.connectionReadyTimer = null;
    }
    this.connectionReadyReject?.(err);
    this.connectionReadyResolve = null;
    this.connectionReadyReject = null;
  }

  private resubscribeChannels(): void {
    // Resubscribe to all channels after reconnect
    for (const channelId of this.subscribedChannels) {
      this.subscribe(channelId);
    }
    // Resubscribe to all servers after reconnect
    for (const serverId of this.subscribedServers) {
      this.subscribeServer(serverId);
    }
    // Resubscribe to all DM conversations after reconnect
    for (const convId of this.subscribedDMs) {
      this.subscribeDM(convId);
    }
    // Emit barrier probe. Because the hub processes incoming frames via a
    // single serialized Run() goroutine, the probe is guaranteed to be
    // handled AFTER every subscribe above is committed to the subscriber map.
    // V1 hubs ignore unknown frame types — backwards compatible.
    this.send({
      type: 'connection_ready_probe',
      data: { protocol_version: 2 },
    });
  }

  private setState(newState: ConnectionState): void {
    if (this.state !== newState) {
      this.state = newState;
      this.notifyConnectionHandlers();
    }
  }

  private notifyConnectionHandlers(): void {
    for (const handler of this.connectionHandlers) {
      try {
        handler(this.state);
      } catch (error) {
        console.error('Error in connection handler:', errorMessage(error));
      }
    }
  }
}

// Singleton instance
let wsService: WebSocketService | null = null;

export const getWebSocketService = (baseUrl?: string): WebSocketService => {
  wsService ??= new WebSocketService(baseUrl);
  return wsService;
};

export default getWebSocketService;
