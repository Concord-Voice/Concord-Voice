/**
 * A chat surface is one panel that pairs a message list with its own composer: the channel
 * view, the DM view, or a voice text chat (#1959). More than one can be mounted at once — the
 * voice text chat side panel and drawer sit beside the channel or DM view, and can show the
 * same channel — so anything that ties a message row to "the" composer must name the surface.
 *
 * Each owner takes a `useId()`, puts it on its root element as `data-chat-surface`, and passes
 * it as the required `surfaceId` prop to its `MessageList` (which hands it to every row) and to
 * its `DeleteRefusalModal`. Required, so the compiler names any new owner that forgets it. The
 * DOM helpers below find that root by comparing the attribute, never by building a selector
 * from the id.
 */
const SURFACE_SELECTOR = '[data-chat-surface]';

function findSurfaceRoot(surfaceId: string): HTMLElement | null {
  for (const root of document.querySelectorAll<HTMLElement>(SURFACE_SELECTOR)) {
    if (root.dataset.chatSurface === surfaceId) return root;
  }
  return null;
}

/** True when `el` belongs to `root` itself rather than to a surface nested inside it. */
function belongsTo(el: Element, root: HTMLElement): boolean {
  return el.closest(SURFACE_SELECTOR) === root;
}

/** The composer textarea of one surface, or null when that surface has none mounted. */
export function findSurfaceComposer(surfaceId: string): HTMLElement | null {
  const root = findSurfaceRoot(surfaceId);
  if (!root) return null;
  for (const composer of root.querySelectorAll<HTMLElement>('.message-input-textarea')) {
    if (belongsTo(composer, root)) return composer;
  }
  return null;
}

/** One message's row inside one surface. Ids are server-issued, so they are compared as
 *  attribute values and never reach `querySelector` as syntax. */
export function findSurfaceMessageRow(surfaceId: string, messageId: string): HTMLElement | null {
  const root = findSurfaceRoot(surfaceId);
  if (!root) return null;
  for (const row of root.querySelectorAll<HTMLElement>('[data-message-id]')) {
    if (row.dataset.messageId === messageId && belongsTo(row, root)) return row;
  }
  return null;
}

/** Moves focus to the surface's own composer. A surface without one is a no-op: focus never
 *  falls back to another panel's composer, where the next message would go to a different
 *  conversation. */
export function focusSurfaceComposer(surfaceId: string): void {
  findSurfaceComposer(surfaceId)?.focus();
}
