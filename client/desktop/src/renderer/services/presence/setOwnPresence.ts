import { getWebSocketService } from '../messaging/websocketService';

/** Presences a user can choose for themselves ('offline' is observed, never chosen). */
export type SettablePresence = 'online' | 'dnd' | 'invisible';

/**
 * Sends the signed-in user's own presence over the WebSocket.
 *
 * The single call site for "the user picked a status", shared by the profile
 * popover and the tray menu (via `trayActionDispatcher`) so the two cannot
 * drift: whatever the popover does on a pick, the tray does too.
 */
export function setOwnPresence(status: SettablePresence): void {
  getWebSocketService().sendSetStatus(status);
}
