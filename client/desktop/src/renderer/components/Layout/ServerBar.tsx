import React, { useRef, useState, useCallback, useEffect, useMemo } from 'react';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import { createPortal } from 'react-dom';
import { useNavigate, useLocation } from 'react-router-dom';
import { MessageSquare, Plus, ChevronLeft, ChevronRight } from 'lucide-react';
import { useServerStore } from '../../stores/serverStore';
import { useUnreadStore } from '../../stores/unreadStore';
import {
  useNotificationPrefsStore,
  isEntryCurrentlyMuted,
} from '../../stores/notificationPrefsStore';
import { useVoiceStore } from '../../stores/voiceStore';
import { useLayoutStore } from '../../stores/layoutStore';
import { useAuthStore } from '../../stores/authStore';
import { useSettingsStore } from '../../stores/settingsStore';
import { ServerWithRole } from '../../types/server';
import './ServerBar.css';

interface ServerBarProps {
  onOpenActionModal: () => void;
  onContextMenu: (server: ServerWithRole, position: { x: number; y: number }) => void;
}

// Dock magnification constants
const MAG_RANGE_PX = 140; // Pixel radius of influence
const MAX_SCALE = 1.4; // Max magnification ratio
const BAR_PADDING = 8; // Vertical padding (4px top + 4px bottom)

/** Cosine bell-curve falloff: 1.0 at center, falls to 0.0 at MAG_RANGE_PX */
function getScale(distPx: number): number {
  if (distPx >= MAG_RANGE_PX) return 1;
  const ratio = distPx / MAG_RANGE_PX;
  const factor = 0.5 * (1 + Math.cos(Math.PI * ratio));
  return 1 + (MAX_SCALE - 1) * factor;
}

const ServerBar: React.FC<ServerBarProps> = ({ onOpenActionModal, onContextMenu }) => {
  const navigate = useNavigate();
  const location = useLocation();
  const scrollRef = useRef<HTMLDivElement>(null);
  // eslint-disable-next-line @eslint-react/naming-convention-ref-name -- stable ref; rename to the *Ref-suffix convention deferred to avoid churning untested handler lines in this low-coverage component (new-code coverage gate). Cosmetic rule suppressed per [internal]rules conventions.
  const iconRefs = useRef<Map<string, HTMLButtonElement>>(new Map());

  const accessToken = useAuthStore((s) => s.accessToken);
  const servers = useServerStore((s) => s.servers);
  const activeServerId = useServerStore((s) => s.activeServerId);
  const isLoading = useServerStore((s) => s.isLoading);
  const fetchServers = useServerStore((s) => s.fetchServers);
  const setActiveServer = useServerStore((s) => s.setActiveServer);
  const serverUnreadSet = useUnreadStore((s) => s.serverUnreadSet);
  const serverVoiceCounts = useVoiceStore((s) => s.serverVoiceCounts);
  // Subscribe to the muted-servers map so this row re-renders when a mute
  // is toggled. We read entries inside the render closure (per-server) to
  // honor the expiry check inline — Map identity already changes when
  // setMute / removeMute fire, so the subscription is sufficient to drive
  // re-renders on every toggle.
  const mutedServers = useNotificationPrefsStore((s) => s.mutedServers);

  const serverFolders = useLayoutStore((s) => s.serverFolders);
  const serverOrder = useLayoutStore((s) => s.serverOrder);
  const reorderServers = useLayoutStore((s) => s.reorderServers);
  const removeServerFromFolder = useLayoutStore((s) => s.removeServerFromFolder);
  const channelPanelPinned = useLayoutStore((s) => s.channelPanelPinned);
  const showChannelPanelHover = useLayoutStore((s) => s.showChannelPanelHover);
  const hideChannelPanelHover = useLayoutStore((s) => s.hideChannelPanelHover);
  const serverBarHeight = useLayoutStore((s) => s.serverBarHeight);
  const reduceAnimations = useSettingsStore((s) => s.appearance.reduceAnimations);

  // Hover-to-open timers for unpinned channel panel
  const hoverShowRef = useRef<NodeJS.Timeout | null>(null);
  const hoverHideRef = useRef<NodeJS.Timeout | null>(null);

  const [showLeftArrow, setShowLeftArrow] = useState(false);
  const [showRightArrow, setShowRightArrow] = useState(false);
  const [dragInsertPos, setDragInsertPos] = useState<{
    targetId: string;
    side: 'before' | 'after';
  } | null>(null);
  const lastInsertRef = useRef<{ targetId: string; side: 'before' | 'after' } | null>(null);
  const [draggingId, setDraggingId] = useState<string | null>(null);
  // eslint-disable-next-line @eslint-react/use-state -- Map() is cheap to construct; lazy initializer would add noise without benefit
  const [magScales, setMagScales] = useState<Map<string, number>>(new Map());
  const [hoveredServer, setHoveredServer] = useState<{
    server: ServerWithRole;
    rect: DOMRect;
  } | null>(null);
  const [addBtnTooltipPos, setAddBtnTooltipPos] = useState<{ top: number; left: number } | null>(
    null
  );
  const addBtnRef = useRef<HTMLButtonElement>(null);
  const [stickyTooltip, setStickyTooltip] = useState<{
    text: string;
    top: number;
    left: number;
  } | null>(null);
  const pmBtnRef = useRef<HTMLButtonElement>(null);
  const activeServerBtnRef = useRef<HTMLButtonElement>(null);

  // Size calculations:
  // maxIconSize = the biggest an icon can get (fills the bar height minus padding)
  // baseIconSize = resting size = maxIconSize / MAX_SCALE so magnified icons never overflow
  const maxIconSize = Math.max(28, serverBarHeight - BAR_PADDING);
  const baseIconSize = Math.round(maxIconSize / MAX_SCALE);
  // Active + PM icons: same as base (they don't magnify, so they sit at resting size)
  const stickyIconSize = Math.round(maxIconSize * 0.85);

  // Fetch servers on mount
  useEffect(() => {
    if (accessToken) fetchServers();
  }, [accessToken, fetchServers]);

  // IDs of servers that are inside folders (excluded from top-level bar)
  const folderedIds = useMemo(() => {
    const set = new Set<string>();
    for (const f of serverFolders) for (const id of f.serverIds) set.add(id);
    return set;
  }, [serverFolders]);

  // Servers shown in the horizontal bar (non-foldered, ordered, excluding active)
  const barServers = useMemo(() => {
    const nonFoldered = servers.filter((s) => !folderedIds.has(s.id) && s.id !== activeServerId);
    if (serverOrder.length === 0) return nonFoldered;

    const ordered: ServerWithRole[] = [];
    const byId = new Map(nonFoldered.map((s) => [s.id, s]));

    for (const id of serverOrder) {
      const server = byId.get(id);
      if (server) {
        ordered.push(server);
        byId.delete(id);
      }
    }

    const remaining = [...byId.values()];
    return [...remaining, ...ordered];
  }, [servers, folderedIds, serverOrder, activeServerId]);

  const activeServer = servers.find((s) => s.id === activeServerId) || null;

  // Check scroll overflow
  const checkOverflow = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: updates scroll arrow visibility based on DOM measurement; called from a scroll/resize listener and from effects
    setShowLeftArrow(el.scrollLeft > 0);
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: updates scroll arrow visibility based on DOM measurement; called from a scroll/resize listener and from effects
    setShowRightArrow(el.scrollLeft + el.clientWidth < el.scrollWidth - 1);
  }, []);

  useEffect(() => {
    checkOverflow();
    const el = scrollRef.current;
    if (el) {
      el.addEventListener('scroll', checkOverflow);
      globalThis.addEventListener('resize', checkOverflow);
    }
    return () => {
      el?.removeEventListener('scroll', checkOverflow);
      globalThis.removeEventListener('resize', checkOverflow);
    };
  }, [checkOverflow, barServers]);

  const scrollBy = (delta: number) => {
    scrollRef.current?.scrollBy({ left: delta, behavior: 'smooth' });
  };

  // Click active server icon → navigate to server view (from DM), toggle pin, or instant-show unpinned panel
  const handleActiveServerClick = () => {
    if (location.pathname !== '/app') {
      navigate('/app');
      return;
    }
    // Already on the server view. When pinned, do nothing — clicking the active
    // server must NOT unpin the panel (#188 feedback: surprising re-collapse,
    // and it would also bypass the interface lock). When unpinned, instant-show
    // the peek overlay (cancel any pending hover timers).
    if (!channelPanelPinned) {
      if (hoverShowRef.current) {
        clearTimeout(hoverShowRef.current);
        hoverShowRef.current = null;
      }
      if (hoverHideRef.current) {
        clearTimeout(hoverHideRef.current);
        hoverHideRef.current = null;
      }
      showChannelPanelHover();
    }
  };

  // Click PM icon — navigate to DMs, or instant-show unpinned panel if already there
  const handlePMClick = () => {
    if (location.pathname !== '/app/dms') {
      navigate('/app/dms');
      return;
    }
    // Same rule as the active-server icon (#188): when pinned, do NOT unpin on
    // click. When unpinned, instant-show the peek overlay.
    if (!channelPanelPinned) {
      if (hoverShowRef.current) {
        clearTimeout(hoverShowRef.current);
        hoverShowRef.current = null;
      }
      if (hoverHideRef.current) {
        clearTimeout(hoverHideRef.current);
        hoverHideRef.current = null;
      }
      showChannelPanelHover();
    }
  };

  // Hover-to-open for unpinned channel panel with intentful delay
  const hoverShowDelay = reduceAnimations ? 0 : 300;
  // Keep a grace period on hide so the user can travel from the icon to the panel
  const hoverHideDelay = reduceAnimations ? 200 : 300;

  const handleActiveServerEnter = useCallback(() => {
    if (channelPanelPinned) return;
    // Clear any pending hide so re-hovering keeps the panel alive
    if (hoverHideRef.current) {
      clearTimeout(hoverHideRef.current);
      hoverHideRef.current = null;
    }
    // Intentful delay — only open if the cursor lingers (instant when reduced)
    hoverShowRef.current = setTimeout(() => {
      showChannelPanelHover();
      hoverShowRef.current = null;
    }, hoverShowDelay);
  }, [channelPanelPinned, showChannelPanelHover, hoverShowDelay]);

  const handleActiveServerLeave = useCallback(() => {
    // Cancel any pending show
    if (hoverShowRef.current) {
      clearTimeout(hoverShowRef.current);
      hoverShowRef.current = null;
    }
    if (channelPanelPinned) return;
    // Give the user time to reach the channel panel before hiding
    hoverHideRef.current = setTimeout(() => {
      hideChannelPanelHover();
      hoverHideRef.current = null;
    }, hoverHideDelay);
  }, [channelPanelPinned, hideChannelPanelHover, hoverHideDelay]);

  // Cleanup hover timers on unmount
  useEffect(() => {
    return () => {
      if (hoverShowRef.current) clearTimeout(hoverShowRef.current);
      if (hoverHideRef.current) clearTimeout(hoverHideRef.current);
    };
  }, []);

  // Click server in bar
  const handleServerClick = (server: ServerWithRole) => {
    setHoveredServer(null); // Clear tooltip before the icon unmounts
    setActiveServer(server.id);
    if (location.pathname !== '/app') navigate('/app');
  };

  // Context menu
  const handleContextMenu = (e: React.MouseEvent, server: ServerWithRole) => {
    e.preventDefault();
    e.stopPropagation();
    onContextMenu(server, { x: e.clientX, y: e.clientY });
  };

  // Drag and drop
  const handleDragStart = (e: React.DragEvent, serverId: string) => {
    e.dataTransfer.setData('text/plain', serverId);
    e.dataTransfer.setData('application/concord-server', serverId);
    e.dataTransfer.effectAllowed = 'move';
    setDraggingId(serverId);

    // Create a smaller, semi-transparent drag image
    const iconEl = iconRefs.current.get(serverId);
    if (iconEl) {
      const clone = iconEl.cloneNode(true) as HTMLElement;
      const size = Math.round(baseIconSize * 0.7);
      Object.assign(clone.style, {
        width: `${size}px`,
        height: `${size}px`,
        opacity: '0.65',
        position: 'fixed',
        top: '-9999px',
        left: '-9999px',
        pointerEvents: 'none',
        borderRadius: '25%',
      });
      document.body.appendChild(clone);
      e.dataTransfer.setDragImage(clone, size / 2, size / 2);
      requestAnimationFrame(() => clone.remove());
    }
  };

  const handleDragOver = (e: React.DragEvent, serverId: string) => {
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    const el = iconRefs.current.get(serverId);
    if (!el) return;

    const rect = el.getBoundingClientRect();
    const midX = rect.left + rect.width / 2;
    const deadZone = rect.width * 0.25;

    // If hovering the same target inside the dead zone, keep the current side
    const last = lastInsertRef.current;
    if (last?.targetId === serverId && Math.abs(e.clientX - midX) < deadZone) {
      return;
    }

    const side: 'before' | 'after' = e.clientX < midX ? 'before' : 'after';
    const newPos = { targetId: serverId, side };
    lastInsertRef.current = newPos;
    setDragInsertPos(newPos);
  };

  // Only clear insert indicator when leaving the scroll container entirely
  const handleScrollDragLeave = (e: React.DragEvent) => {
    if (!(e.currentTarget as HTMLElement).contains(e.relatedTarget as Node)) {
      setDragInsertPos(null);
      lastInsertRef.current = null;
    }
  };

  const handleDrop = (e: React.DragEvent, targetId: string) => {
    e.preventDefault();
    e.stopPropagation();
    const insertPos = dragInsertPos;
    setDragInsertPos(null);
    lastInsertRef.current = null;
    const sourceId = e.dataTransfer.getData('text/plain');
    if (!sourceId || sourceId === targetId) return;

    // If source is in a folder, remove it first
    const isInBar = barServers.some((s) => s.id === sourceId);
    if (!isInBar) {
      removeServerFromFolder(sourceId);
    }

    const currentOrder = barServers.map((s) => s.id);
    const filtered = currentOrder.filter((id) => id !== sourceId);
    const targetIdx = filtered.indexOf(targetId);
    if (targetIdx === -1) return;

    const insertIdx = insertPos?.side === 'after' ? targetIdx + 1 : targetIdx;
    filtered.splice(insertIdx, 0, sourceId);
    reorderServers(filtered);
  };

  // Drop on bar area (ghost, gap, or empty space)
  const handleBarDrop = (e: React.DragEvent) => {
    e.preventDefault();
    const insertPos = dragInsertPos;
    setDragInsertPos(null);
    lastInsertRef.current = null;
    const sourceId = e.dataTransfer.getData('text/plain');
    if (!sourceId) return;

    const isInBar = barServers.some((s) => s.id === sourceId);
    if (!isInBar) {
      removeServerFromFolder(sourceId);
    }

    // Use the last known insert position to reorder
    if (insertPos) {
      const currentOrder = barServers.map((s) => s.id);
      const filtered = currentOrder.filter((id) => id !== sourceId);
      const targetIdx = filtered.indexOf(insertPos.targetId);
      if (targetIdx !== -1) {
        const insertIdx = insertPos.side === 'after' ? targetIdx + 1 : targetIdx;
        filtered.splice(insertIdx, 0, sourceId);
        reorderServers(filtered);
      }
    }
  };

  const handleDragEnd = useCallback(() => {
    setDragInsertPos(null);
    lastInsertRef.current = null;
    setDraggingId(null);
    setMagScales(new Map());
    setHoveredServer(null);
  }, []);

  // Clear dragging state if the dragged server leaves the bar (e.g. moved to folder)
  // — the unmounted element can't fire onDragEnd, so we clean up here
  useEffect(() => {
    if (draggingId && !barServers.some((s) => s.id === draggingId)) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: cleans up draggingId when the dragged server leaves the bar; the unmounted element can't fire onDragEnd
      setDraggingId(null);
    }
  }, [barServers, draggingId]);

  // Clear tooltip if the hovered server is no longer in the bar (e.g. it became active)
  // — the unmounted element can't fire onMouseLeave, so we clean up here
  useEffect(() => {
    if (hoveredServer && !barServers.some((s) => s.id === hoveredServer.server.id)) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears hoveredServer tooltip when server leaves the bar; the unmounted element can't fire onMouseLeave
      setHoveredServer(null);
    }
  }, [barServers, hoveredServer]);

  // Dock magnification — compute per-icon scale from cursor distance
  const handleMouseMove = useCallback(
    (e: React.MouseEvent) => {
      if (reduceAnimations) return;
      const scales = new Map<string, number>();
      for (const [id, el] of iconRefs.current) {
        const rect = el.getBoundingClientRect();
        const centerX = rect.left + rect.width / 2;
        const dist = Math.abs(e.clientX - centerX);
        scales.set(id, getScale(dist));
      }
      setMagScales(scales);
    },
    [reduceAnimations]
  );

  const handleMouseLeave = useCallback(() => {
    setMagScales(new Map());
  }, []);

  // Add Server button tooltip (portal-based, instant)
  const showAddBtnTooltip = useCallback(() => {
    if (addBtnRef.current) {
      const rect = addBtnRef.current.getBoundingClientRect();
      const tooltipWidth = 80;
      const padding = 8;
      const top = rect.bottom + 6;
      let left = rect.left + rect.width / 2 - tooltipWidth / 2;
      left = Math.max(padding, Math.min(left, globalThis.innerWidth - tooltipWidth - padding));
      const clampedTop = Math.min(top, globalThis.innerHeight - 30 - padding);
      setAddBtnTooltipPos({ top: clampedTop, left });
    }
  }, []);

  const hideAddBtnTooltip = useCallback(() => {
    setAddBtnTooltipPos(null);
  }, []);

  // Sticky icon tooltips (PM + Active Server) — portal-based, instant
  const showStickyTooltip = useCallback(
    (text: string, ref: React.RefObject<HTMLButtonElement | null>) => {
      const el = ref.current;
      if (el) {
        const rect = el.getBoundingClientRect();
        const padding = 8;
        const tooltipWidth = text.length * 7 + 16; // rough estimate
        const top = rect.bottom + 6;
        let left = rect.left + rect.width / 2 - tooltipWidth / 2;
        left = Math.max(padding, Math.min(left, globalThis.innerWidth - tooltipWidth - padding));
        const clampedTop = Math.min(top, globalThis.innerHeight - 30 - padding);
        setStickyTooltip({ text, top: clampedTop, left });
      }
    },
    []
  );

  const hideStickyTooltip = useCallback(() => {
    setStickyTooltip(null);
  }, []);

  /** Get the magnified pixel size for a server icon */
  const getMagSize = (serverId: string): number => {
    const scale = magScales.get(serverId);
    if (!scale || scale <= 1) return baseIconSize;
    return Math.round(baseIconSize * scale);
  };

  return (
    <div className="server-bar">
      {/* Sticky left: PM + Active server */}
      <div className="server-bar-sticky">
        <button
          ref={pmBtnRef}
          className={`server-bar-pm-icon${location.pathname === '/app/dms' ? ' active' : ''}`}
          aria-label="Direct Messages"
          onClick={handlePMClick}
          onMouseEnter={() => {
            showStickyTooltip('Direct Messages', pmBtnRef);
            if (location.pathname === '/app/dms') handleActiveServerEnter();
          }}
          onMouseLeave={() => {
            hideStickyTooltip();
            if (location.pathname === '/app/dms') handleActiveServerLeave();
          }}
          style={{ width: stickyIconSize, height: stickyIconSize }}
        >
          <MessageSquare size={Math.round(stickyIconSize * 0.45)} />
        </button>

        {activeServer ? (
          <button
            ref={activeServerBtnRef}
            className={`server-bar-active-icon${location.pathname === '/app/dms' ? ' inactive' : ''}`}
            onClick={handleActiveServerClick}
            onMouseEnter={() => {
              showStickyTooltip(activeServer.name, activeServerBtnRef);
              if (location.pathname !== '/app/dms') handleActiveServerEnter();
            }}
            onMouseLeave={() => {
              hideStickyTooltip();
              if (location.pathname !== '/app/dms') handleActiveServerLeave();
            }}
            onContextMenu={(e) => handleContextMenu(e, activeServer)}
            aria-label="Toggle channel panel"
            style={{ width: stickyIconSize, height: stickyIconSize }}
          >
            {resolveMediaUrl(activeServer.icon_url) ? (
              <img src={resolveMediaUrl(activeServer.icon_url)} alt={activeServer.name} />
            ) : (
              <span className="server-bar-icon-initial">
                {activeServer.name.charAt(0).toUpperCase()}
              </span>
            )}
          </button>
        ) : (
          <div
            className="server-bar-empty-server"
            aria-label="No active server"
            style={{ width: stickyIconSize, height: stickyIconSize }}
          />
        )}
      </div>

      <div
        className="server-bar-divider"
        style={{
          marginRight:
            barServers.length > 0 && !reduceAnimations
              ? 4 + Math.max(0, (getMagSize(barServers[0].id) - baseIconSize) / 2)
              : 4,
        }}
      />

      {/* Scrollable server list */}
      <div
        className="server-bar-scroll-container"
        onMouseMove={handleMouseMove}
        onMouseLeave={handleMouseLeave}
      >
        {showLeftArrow && (
          <button
            className="server-bar-scroll-arrow left"
            onClick={() => scrollBy(-200)}
            aria-label="Scroll left"
          >
            <ChevronLeft size={14} />
          </button>
        )}

        <div
          className="server-bar-scroll"
          ref={scrollRef}
          onDragOver={(e) => {
            e.preventDefault();
            e.dataTransfer.dropEffect = 'move';
          }}
          onDragLeave={handleScrollDragLeave}
          onDrop={handleBarDrop}
        >
          {/* Loading skeletons */}
          {isLoading && servers.length === 0 && (
            <>
              <div
                className="server-bar-icon skeleton"
                style={{ width: baseIconSize, height: baseIconSize, opacity: 0.4 }}
              />
              <div
                className="server-bar-icon skeleton"
                style={{ width: baseIconSize, height: baseIconSize, opacity: 0.3 }}
              />
              <div
                className="server-bar-icon skeleton"
                style={{ width: baseIconSize, height: baseIconSize, opacity: 0.2 }}
              />
            </>
          )}

          {/* Server icons */}
          {barServers.map((server) => {
            const hasUnread = serverUnreadSet.has(server.id);
            // Inline mute-state check via the shared store helper so we
            // honor `mutedUntil` expiry without waiting for the 60s sweep.
            // A muted server gets a data-muted attribute and a corner icon;
            // see ServerBar.css for the visual treatment.
            const isMuted = isEntryCurrentlyMuted(mutedServers.get(server.id));
            const magSize = getMagSize(server.id);
            const isScaled = magSize > baseIconSize;
            const ghostBefore =
              dragInsertPos?.targetId === server.id && dragInsertPos?.side === 'before';
            const ghostAfter =
              dragInsertPos?.targetId === server.id && dragInsertPos?.side === 'after';

            const ghost = (
              <div
                key={`ghost-${server.id}`}
                className="server-bar-icon-ghost"
                style={{ width: baseIconSize, height: baseIconSize }}
              />
            );

            return (
              <React.Fragment key={server.id}>
                {ghostBefore && ghost}
                <div
                  className={`server-bar-icon-wrapper ${draggingId === server.id ? 'dragging' : ''}`}
                  draggable
                  onDragStart={(e) => {
                    handleDragStart(e, server.id);
                    setHoveredServer(null);
                  }}
                  onDragEnd={handleDragEnd}
                  onDragOver={(e) => handleDragOver(e, server.id)}
                  onDrop={(e) => handleDrop(e, server.id)}
                  onMouseEnter={() => {
                    if (draggingId) return; // Suppress tooltip while dragging
                    const el = iconRefs.current.get(server.id);
                    if (el) setHoveredServer({ server, rect: el.getBoundingClientRect() });
                  }}
                  onMouseLeave={() => setHoveredServer(null)}
                  style={{
                    width: magSize,
                    height: magSize,
                    transition: isScaled
                      ? 'width 0.08s ease-out, height 0.08s ease-out'
                      : 'width 0.2s ease-out, height 0.2s ease-out',
                  }}
                >
                  <button
                    ref={(el) => {
                      if (el) iconRefs.current.set(server.id, el);
                      else iconRefs.current.delete(server.id);
                    }}
                    className="server-bar-icon"
                    data-muted={isMuted ? 'true' : undefined}
                    draggable={false}
                    style={{ width: magSize, height: magSize }}
                    onClick={() => handleServerClick(server)}
                    onContextMenu={(e) => handleContextMenu(e, server)}
                    aria-label={`${server.name} server${isMuted ? ' (muted)' : ''}`}
                  >
                    {resolveMediaUrl(server.icon_url) ? (
                      <img
                        src={resolveMediaUrl(server.icon_url)}
                        alt={server.name}
                        draggable={false}
                      />
                    ) : (
                      <span className="server-bar-icon-initial">
                        {server.name.charAt(0).toUpperCase()}
                      </span>
                    )}
                  </button>

                  {hasUnread && <span className="server-bar-badge" />}
                  {isMuted && (
                    // Small bell-with-slash overlay in the corner. The icon
                    // sits absolutely over the bottom-right; CSS handles
                    // sizing relative to the dynamic magSize so it scales
                    // with hover-zoom.
                    <span className="server-bar-mute-overlay" aria-hidden="true">
                      <svg viewBox="0 0 12 12" width="100%" height="100%" fill="none">
                        <circle cx="6" cy="6" r="6" fill="var(--bg-secondary)" />
                        <path
                          d="M4 4.5a2 2 0 014 0v2l.5.5h-5l.5-.5v-2z"
                          stroke="currentColor"
                          strokeWidth="0.9"
                          strokeLinejoin="round"
                          fill="none"
                        />
                        <path
                          d="M3 3l6 6"
                          stroke="currentColor"
                          strokeWidth="0.9"
                          strokeLinecap="round"
                        />
                      </svg>
                    </span>
                  )}
                </div>
                {ghostAfter && ghost}
              </React.Fragment>
            );
          })}
        </div>

        {showRightArrow && (
          <button
            className="server-bar-scroll-arrow right"
            onClick={() => scrollBy(200)}
            aria-label="Scroll right"
          >
            <ChevronRight size={14} />
          </button>
        )}
      </div>

      {/* Sticky right: Add server */}
      <div
        className="server-bar-divider"
        style={{
          marginLeft:
            barServers.length > 0 && !reduceAnimations
              ? 4 +
                Math.max(0, (getMagSize(barServers[barServers.length - 1].id) - baseIconSize) / 2)
              : 4,
        }}
      />

      <div className="server-bar-sticky">
        <button
          ref={addBtnRef}
          className="server-bar-add"
          onClick={onOpenActionModal}
          aria-label="Add Server"
          onMouseEnter={showAddBtnTooltip}
          onMouseLeave={hideAddBtnTooltip}
          style={{ width: stickyIconSize, height: stickyIconSize }}
        >
          <Plus size={Math.round(stickyIconSize * 0.45)} />
        </button>
      </div>

      {/* Portal tooltip — rendered outside overflow:hidden containers */}
      {hoveredServer &&
        createPortal(
          <div
            className="server-bar-tooltip-fixed"
            style={{
              position: 'fixed',
              top: hoveredServer.rect.bottom + 8,
              left: hoveredServer.rect.left + hoveredServer.rect.width / 2,
              transform: 'translateX(-50%)',
            }}
          >
            <span className="server-bar-tooltip-name">{hoveredServer.server.name}</span>
            <div className="server-bar-tooltip-stats">
              <span>{hoveredServer.server.member_count ?? 0} Members</span>
              <span className="server-bar-tooltip-dot" />
              <span>{hoveredServer.server.online_count ?? 0} Online</span>
            </div>
            <div className="server-bar-tooltip-stats">
              <span
                className={`server-bar-tooltip-voice${(serverVoiceCounts[hoveredServer.server.id] ?? 0) > 0 ? ' server-bar-tooltip-voice--active' : ''}`}
              >
                {serverVoiceCounts[hoveredServer.server.id] ?? 0} In Voice
              </span>
            </div>
            {serverUnreadSet.has(hoveredServer.server.id) && (
              <span className="server-bar-tooltip-unread">Unread notifications</span>
            )}
          </div>,
          document.body
        )}

      {/* "Add Server" tooltip — portaled to body to escape overflow:hidden */}
      {addBtnTooltipPos &&
        createPortal(
          <div
            className="server-bar-add-tooltip visible"
            style={{ top: addBtnTooltipPos.top, left: addBtnTooltipPos.left }}
          >
            Add Server
          </div>,
          document.body
        )}

      {/* Sticky icon tooltip (DM / Active Server) */}
      {stickyTooltip &&
        createPortal(
          <div
            className="server-bar-add-tooltip visible"
            style={{ top: stickyTooltip.top, left: stickyTooltip.left }}
          >
            {stickyTooltip.text}
          </div>,
          document.body
        )}
    </div>
  );
};

export default ServerBar;
