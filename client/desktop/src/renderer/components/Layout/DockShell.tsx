import React, {
  createContext,
  use,
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
} from 'react';
import { ChevronLeft, ChevronRight, Pin } from 'lucide-react';
import { useResizablePanel } from '../../hooks/useResizablePanel';
import { useSettingsStore } from '../../stores/settingsStore';
import {
  LEFT_SIDEBAR_MAX_WIDTH,
  RIGHT_SIDEBAR_MAX_WIDTH,
  SIDEBAR_COMPACT_BREAKPOINT,
  SIDEBAR_MIN_WIDTH,
  selectSidebarDock,
  type SidebarContext,
  type SidebarSide,
  useLayoutStore,
} from '../../stores/layoutStore';
import './AppLayout.css';

interface DockOverlayContextValue {
  activeOverlayId: string | null;
  openOverlay: (id: string) => void;
  closeOverlay: (id: string) => void;
}

const DockOverlayContext = createContext<DockOverlayContextValue | null>(null);

interface DockOverlayProviderProps {
  children: React.ReactNode;
}

export const DockOverlayProvider: React.FC<DockOverlayProviderProps> = ({ children }) => {
  const [activeOverlayId, setActiveOverlayId] = useState<string | null>(null);
  const openOverlay = useCallback((id: string) => setActiveOverlayId(id), []);
  const closeOverlay = useCallback(
    (id: string) => setActiveOverlayId((active) => (active === id ? null : active)),
    []
  );
  const value = useMemo(
    () => ({ activeOverlayId, openOverlay, closeOverlay }),
    [activeOverlayId, closeOverlay, openOverlay]
  );

  return <DockOverlayContext value={value}>{children}</DockOverlayContext>;
};

export interface DockShellProps {
  context: SidebarContext;
  side: SidebarSide;
  label: string;
  header: React.ReactNode | ((compact: boolean) => React.ReactNode);
  renderBody: (compact: boolean) => React.ReactNode;
  footer?: React.ReactNode | ((compact: boolean) => React.ReactNode);
  forcePinned?: boolean;
}

interface DockLipProps {
  side: SidebarSide;
  label: string;
  effectivePinned: boolean;
  lipCovered: boolean;
  showOverlay: boolean;
  lipFocused: boolean;
  lipRef: React.RefObject<HTMLButtonElement | null>;
  onOpen: (heldOpen: boolean) => void;
  onFocus: () => void;
  onBlur: () => void;
}

interface DockPinButtonProps {
  forcePinned: boolean;
  interfaceLocked: boolean;
  effectivePinned: boolean;
  label: string;
  onToggle: () => void;
}

interface DockResizeHandleProps {
  interfaceLocked: boolean;
  label: string;
  width: number;
  maxWidth: number;
  onMouseDown: (event: React.MouseEvent) => void;
  onKeyDown: (event: React.KeyboardEvent) => void;
}

interface DockSurfaceProps {
  side: SidebarSide;
  effectivePinned: boolean;
  showOverlay: boolean;
  compact: boolean;
  header: DockShellProps['header'];
  headerContent: React.ReactNode;
  footerContent: React.ReactNode;
  pinButton: React.ReactNode;
  showPinButton: boolean;
  resizeHandle: React.ReactNode;
  width: number;
  isResizing: boolean;
  surfaceRef: React.RefObject<HTMLDivElement | null>;
  renderBody: DockShellProps['renderBody'];
  onMouseEnter: () => void;
  onMouseLeave: () => void;
}

const DockLip: React.FC<DockLipProps> = ({
  side,
  label,
  effectivePinned,
  lipCovered,
  showOverlay,
  lipFocused,
  lipRef,
  onOpen,
  onFocus,
  onBlur,
}) => {
  if (effectivePinned) return null;
  return (
    <button
      type="button"
      className={`dock-shell__lip${lipCovered ? ' dock-shell__lip--covered' : ''}${showOverlay && lipFocused ? ' dock-shell__lip--focused' : ''}`}
      ref={lipRef}
      onMouseEnter={() => onOpen(false)}
      onFocus={onFocus}
      onBlur={onBlur}
      onClick={() => onOpen(true)}
      aria-label={`Open ${label} sidebar`}
      aria-hidden={lipCovered}
      tabIndex={lipCovered ? -1 : 0}
    >
      {side === 'left' ? <ChevronRight size={14} /> : <ChevronLeft size={14} />}
    </button>
  );
};

const DockPinButton: React.FC<DockPinButtonProps> = ({
  forcePinned,
  interfaceLocked,
  effectivePinned,
  label,
  onToggle,
}) => {
  if (forcePinned || interfaceLocked) return null;
  const actionLabel = `${effectivePinned ? 'Unpin' : 'Pin'} ${label} sidebar`;
  return (
    <button
      type="button"
      className={`dock-shell__pin${effectivePinned ? ' dock-shell__pin--pinned' : ''}`}
      onClick={onToggle}
      aria-label={actionLabel}
      title={actionLabel}
      aria-pressed={effectivePinned}
    >
      <Pin className="dock-shell__pin-icon" size={14} />
    </button>
  );
};

const DockResizeHandle: React.FC<DockResizeHandleProps> = ({
  interfaceLocked,
  label,
  width,
  maxWidth,
  onMouseDown,
  onKeyDown,
}) => {
  if (interfaceLocked) return null;
  return (
    <div
      className="dock-shell__resize layout-resize-handle"
      role="separator"
      tabIndex={0}
      onMouseDown={onMouseDown}
      onKeyDown={onKeyDown}
      aria-label={`Resize ${label} sidebar`}
      aria-orientation="vertical"
      aria-valuemin={SIDEBAR_MIN_WIDTH}
      aria-valuemax={maxWidth}
      aria-valuenow={width}
    />
  );
};

const DockHeader: React.FC<
  Pick<DockSurfaceProps, 'header' | 'headerContent' | 'pinButton' | 'showPinButton' | 'compact'>
> = ({ header, headerContent, pinButton, showPinButton, compact }) => {
  if (headerContent === null && !showPinButton) return null;
  return (
    <div
      className={`dock-shell__header${header === null ? ' dock-shell__header--actions-only' : ''}`}
      data-layout={compact ? 'vertical' : 'horizontal'}
    >
      {headerContent}
      {pinButton}
    </div>
  );
};

const DockSurface: React.FC<DockSurfaceProps> = ({
  side,
  effectivePinned,
  showOverlay,
  compact,
  header,
  headerContent,
  footerContent,
  pinButton,
  showPinButton,
  resizeHandle,
  width,
  isResizing,
  surfaceRef,
  renderBody,
  onMouseEnter,
  onMouseLeave,
}) => {
  const open = effectivePinned || showOverlay;
  return (
    <div
      className={`dock-shell__surface dock-shell__surface--${effectivePinned ? 'pinned' : 'overlay'}`}
      data-mode={effectivePinned ? 'pinned' : 'overlay'}
      data-state={open ? 'open' : 'closed'}
      data-presentation={compact ? 'compact' : 'standard'}
      data-resizing={String(isResizing)}
      ref={surfaceRef}
      style={{ width }}
      onMouseEnter={onMouseEnter}
      onMouseLeave={onMouseLeave}
      aria-hidden={!open}
      inert={!open}
    >
      {side === 'right' && resizeHandle}
      <DockHeader
        header={header}
        headerContent={headerContent}
        pinButton={pinButton}
        showPinButton={showPinButton}
        compact={compact}
      />
      <div className="dock-shell__body">{renderBody(compact)}</div>
      {footerContent !== undefined && <div className="dock-shell__footer">{footerContent}</div>}
      {side === 'left' && resizeHandle}
    </div>
  );
};

const HIDE_DELAY_MS = 1000;

const isOwnedPortalTarget = (shell: HTMLElement | null, target: EventTarget | null): boolean => {
  if (!shell || !(target instanceof Element)) return false;

  const portal = target.closest<HTMLElement>('[data-dock-focus-owner]');
  const ownerId = portal?.dataset.dockFocusOwner;
  if (!ownerId) return false;

  const owner = document.getElementById(ownerId);
  return owner !== null && shell.contains(owner);
};

export const DockShell: React.FC<DockShellProps> = ({
  context,
  side,
  label,
  header,
  renderBody,
  footer,
  forcePinned = false,
}) => {
  const overlay = use(DockOverlayContext);
  if (!overlay) throw new Error('DockShell must be rendered inside DockOverlayProvider');

  const dock = useLayoutStore((state) => selectSidebarDock(state, context, side));
  const setSidebarPinned = useLayoutStore((state) => state.setSidebarPinned);
  const setSidebarWidth = useLayoutStore((state) => state.setSidebarWidth);
  const interfaceLocked = useLayoutStore((state) => state.interfaceLocked);
  const reduceAnimations = useSettingsStore((state) => state.appearance.reduceAnimations);
  const maxWidth = side === 'left' ? LEFT_SIDEBAR_MAX_WIDTH : RIGHT_SIDEBAR_MAX_WIDTH;
  const panel = useResizablePanel({
    width: dock.width,
    minWidth: SIDEBAR_MIN_WIDTH,
    maxWidth,
    side,
    disabled: interfaceLocked,
    onWidthCommit: (width) => setSidebarWidth(context, side, width),
  });

  const overlayId = useId();
  const shellRef = useRef<HTMLDivElement>(null);
  const surfaceRef = useRef<HTMLDivElement>(null);
  const lipRef = useRef<HTMLButtonElement>(null);
  const hideTimerRef = useRef<NodeJS.Timeout | null>(null);
  const heldOpenRef = useRef(false);
  const restoreLipFocusRef = useRef(false);
  const suppressLipOpenRef = useRef(false);
  const [lipFocused, setLipFocused] = useState(false);
  const effectivePinned = forcePinned || dock.pinned;
  const showOverlay = !effectivePinned && overlay.activeOverlayId === overlayId;
  const lipCovered = showOverlay && !lipFocused;
  const compact = panel.width < SIDEBAR_COMPACT_BREAKPOINT;
  const headerContent = typeof header === 'function' ? header(compact) : header;
  const footerContent = typeof footer === 'function' ? footer(compact) : footer;

  const clearHideTimer = useCallback(() => {
    if (!hideTimerRef.current) return;
    clearTimeout(hideTimerRef.current);
    hideTimerRef.current = null;
  }, []);

  const closeTransientOverlay = useCallback(
    (restoreFocus: boolean) => {
      clearHideTimer();
      heldOpenRef.current = false;
      restoreLipFocusRef.current = restoreFocus;
      overlay.closeOverlay(overlayId);
    },
    [clearHideTimer, overlay, overlayId]
  );

  useEffect(() => {
    if (!showOverlay) return;

    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return;
      event.preventDefault();
      event.stopPropagation();
      closeTransientOverlay(true);
    };
    const handleOutside = (event: MouseEvent) => {
      const shell = shellRef.current;
      if (
        event.target instanceof Node &&
        (shell?.contains(event.target) || isOwnedPortalTarget(shell, event.target))
      ) {
        return;
      }
      closeTransientOverlay(false);
    };

    document.addEventListener('keydown', handleKeyDown);
    document.addEventListener('mousedown', handleOutside);
    return () => {
      document.removeEventListener('keydown', handleKeyDown);
      document.removeEventListener('mousedown', handleOutside);
    };
  }, [closeTransientOverlay, showOverlay]);

  useEffect(() => clearHideTimer, [clearHideTimer]);

  useEffect(() => {
    if (effectivePinned || showOverlay) return;
    const activeElement = document.activeElement;
    const focusIsClosingWithSurface =
      activeElement instanceof Node &&
      (surfaceRef.current?.contains(activeElement) ||
        isOwnedPortalTarget(shellRef.current, activeElement));
    if (!restoreLipFocusRef.current && !focusIsClosingWithSurface) return;

    restoreLipFocusRef.current = false;
    suppressLipOpenRef.current = true;
    lipRef.current?.focus({ preventScroll: true });
    suppressLipOpenRef.current = false;
  }, [effectivePinned, showOverlay]);

  const openTransientOverlay = useCallback(
    (heldOpen: boolean) => {
      clearHideTimer();
      heldOpenRef.current = heldOpen;
      overlay.openOverlay(overlayId);
    },
    [clearHideTimer, overlay, overlayId]
  );

  const handleLipFocus = useCallback(() => {
    setLipFocused(true);
    if (suppressLipOpenRef.current) return;
    openTransientOverlay(false);
  }, [openTransientOverlay]);

  const schedulePointerClose = useCallback(() => {
    clearHideTimer();
    if (heldOpenRef.current) return;
    hideTimerRef.current = setTimeout(
      () => {
        hideTimerRef.current = null;
        const shell = shellRef.current;
        if (
          shell?.contains(document.activeElement) ||
          isOwnedPortalTarget(shell, document.activeElement)
        ) {
          return;
        }
        overlay.closeOverlay(overlayId);
      },
      reduceAnimations ? 0 : HIDE_DELAY_MS
    );
  }, [clearHideTimer, overlay, overlayId, reduceAnimations]);

  const handleBlur = useCallback(
    (event: React.FocusEvent<HTMLDivElement>) => {
      const shell = shellRef.current;
      if (
        heldOpenRef.current ||
        shell?.contains(event.relatedTarget) ||
        isOwnedPortalTarget(shell, event.relatedTarget)
      ) {
        return;
      }
      closeTransientOverlay(false);
    },
    [closeTransientOverlay]
  );

  const togglePinned = useCallback(() => {
    if (interfaceLocked || forcePinned) return;
    heldOpenRef.current = false;
    overlay.closeOverlay(overlayId);
    setSidebarPinned(context, side, !dock.pinned);
  }, [
    context,
    dock.pinned,
    forcePinned,
    interfaceLocked,
    overlay,
    overlayId,
    setSidebarPinned,
    side,
  ]);

  const showPinButton = !forcePinned && !interfaceLocked;
  const pinButton = (
    <DockPinButton
      forcePinned={forcePinned}
      interfaceLocked={interfaceLocked}
      effectivePinned={effectivePinned}
      label={label}
      onToggle={togglePinned}
    />
  );
  const resizeHandle = (
    <DockResizeHandle
      interfaceLocked={interfaceLocked}
      label={label}
      width={panel.width}
      maxWidth={maxWidth}
      onMouseDown={panel.onMouseDown}
      onKeyDown={panel.onKeyDown}
    />
  );

  return (
    <div
      className="dock-shell"
      data-side={side}
      data-reduce-motion={String(reduceAnimations)}
      ref={shellRef}
      onBlur={handleBlur}
    >
      <DockLip
        side={side}
        label={label}
        effectivePinned={effectivePinned}
        lipCovered={lipCovered}
        showOverlay={showOverlay}
        lipFocused={lipFocused}
        lipRef={lipRef}
        onOpen={openTransientOverlay}
        onFocus={handleLipFocus}
        onBlur={() => setLipFocused(false)}
      />
      <DockSurface
        side={side}
        effectivePinned={effectivePinned}
        showOverlay={showOverlay}
        compact={compact}
        header={header}
        headerContent={headerContent}
        footerContent={footerContent}
        pinButton={pinButton}
        showPinButton={showPinButton}
        resizeHandle={resizeHandle}
        width={panel.width}
        isResizing={panel.isResizing}
        surfaceRef={surfaceRef}
        renderBody={renderBody}
        onMouseEnter={clearHideTimer}
        onMouseLeave={schedulePointerClose}
      />
    </div>
  );
};
